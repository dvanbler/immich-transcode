# immich-transcode

Finds large, inefficiently encoded videos in an Immich v3 library and replaces
each with an HEVC (x265) re-encode. For every video it keeps a verified local
backup of the original, and it preserves the timeline date, location, albums,
favorites, description, rating, tags, visibility, shared links and stacks.

The default is a dry run: nothing is uploaded or deleted without `--apply`, and
originals are only ever moved to the Immich trash, never purged.

## Requirements

- Immich server **v3.0 or later**. The client is generated from the v3.2.4 API
  spec and tested against v3.2.4.
- `ffmpeg` and `ffprobe` with libx265, plus `exiftool`. The
  [BtbN static builds](https://github.com/BtbN/FFmpeg-Builds/releases) are
  recommended: they include libx265 and libvmaf. On Linux Mint,
  `apt install ffmpeg libimage-exiftool-perl` also works.
- An Immich API key in `IMMICH_API_KEY`, created under Account Settings → API
  Keys. It needs these permissions:

  | Mode | Permissions |
  |---|---|
  | `scan` | `asset.read`, `user.read` |
  | `run` / `resume` (dry run) | the above plus `asset.download` |
  | `run --apply` / `resume --apply` | the above plus `asset.upload`, `asset.update`, `asset.copy`, `asset.delete`, `tag.asset` |

  `user.read` is needed because search results include partner assets, which
  are excluded by owner. `tag.asset` is needed to put tags back on the
  replacement. At startup the tool lists any permissions that are missing.

## Build

```sh
make build        # static binary, no cgo
make dist         # linux/darwin/windows binaries in dist/
```

## Usage

```sh
cp config.example.yaml ~/.config/immich-transcode/config.yaml   # then edit
export IMMICH_API_KEY=...

immich-transcode scan                      # ranked candidates, read-only
immich-transcode run --limit 3             # dry run: download, encode, validate
immich-transcode run --limit 3 --apply     # replace them in Immich
immich-transcode status                    # manifest: state, savings, failures
immich-transcode resume --apply            # continue anything left partway
immich-transcode reset --state skipped_corrupt   # re-check skipped assets next run
```

Every setting in the config file can also be set by a flag (`--server`,
`--backup-dir`, `--work-dir`, `--preset`, `--concurrency`, `--ffmpeg`, ...).
`run` also accepts `--crf`, `--min-size 500MB`, `--asset <id>` (repeatable),
`--allow-face-loss` and `--retry-failed`. Run `immich-transcode <cmd> --help`
for the full list.

A dry run does all the local work: it downloads the backup, encodes the video
and runs every validation. The encode is kept in `work_dir` (default
`<backup_dir>/work`, so it survives a reboot). A later `--apply` run uploads
that encode instead of encoding again.

Repeating a dry run doesn't redo anything. Assets already validated are
skipped and don't count toward `--limit`, so `run --limit 5` run twice covers
ten different videos. An existing encode keeps its CRF even if the config's
`crf` changes. Passing `--crf` explicitly re-encodes it from the backup at the
new value. The output shows the CRF of each result.

### Re-checking skipped assets

Skipped assets are never selected again. Use `reset` to send them back to
`pending`, for example after changing the `bpp` thresholds or after a fix to a
check. It changes only the manifest: the next run verifies the existing
backup by checksum instead of downloading it, then re-runs every check.
`reset` also accepts `failed` and specific `--asset` ids. Assets that are
`done` can't be reset.

### Ctrl-C

The first Ctrl-C lets the current step finish and then stops. A second one
aborts the step and kills ffmpeg, deletes any partial file and leaves the
asset at its last completed state. Server calls in flight always complete, so
the manifest matches what Immich holds. Use `resume` to continue.

## How it works

`scan` pages through `POST /api/search/metadata` using the v3.2 `filter`
object. The server filters on type, size, visibility (timeline and archive)
and offline status. The client then drops partner, trashed, external-library
and edited assets, plus anything the manifest marks as finished. It estimates
bits per pixel from size ÷ duration ÷ (width × height × 30) and ranks
candidates by estimated savings.

Each asset then moves through these states, recorded in a SQLite manifest. A
failure stops that asset only.

| State | Step |
|---|---|
| `downloaded` | Original streamed to `backup/YYYY/MM/<id>_<name>` and checked against Immich's SHA-1. An existing backup is reused if it matches; a mismatch is never overwritten. |
| `decode_ok` | `ffmpeg -v error … -enc_time_base -1 -f null -` decodes the original. Any output → `skipped_corrupt`. Keeping the source time base avoids false alarms on variable-frame-rate phone video. |
| `probed` | ffprobe computes real bpp. AV1 → `skipped_codec`. H.264 < 0.15 or HEVC < 0.12 → `skipped_efficient`. 10-bit, Main10, PQ or HLG → `skipped_hdr`. |
| `transcoded` | libx265 to `work_dir`, audio copied. Full-range sources (`yuvj*` or `color_range=pc`) keep full range. |
| `metadata_copied` | `exiftool -TagsFromFile` copies the QuickTime, Keys and UserData groups. |
| `validated` | Output decodes cleanly. Duration within 0.5 s, same displayed size and orientation, same video packet count, same CreateDate and GPS. If it isn't at least `min_savings_percent` smaller → `skipped_low_gain`. **A dry run stops here.** |
| `uploaded` | `POST /api/assets` with the original's `fileCreatedAt`, `fileModifiedAt`, filename, favorite and visibility. A duplicate response for an earlier upload of this encode is adopted, and restored from the trash if needed. |
| `copied` | `PUT /api/assets/copy`: albums, favorite, shared links, stack, sidecar. |
| `patched` | Waits for metadata extraction, then fixes any remaining difference in description, rating, date and location (normally none), and adds missing tags. |
| `verified` | `localDateTime` and location must match the original. |
| `trashed` | Named people are checked again, then the original is moved to the trash (`force=false`). |
| `done` | Work file removed. Sizes and CRF recorded. |

**Named people.** Face assignments don't transfer to the replacement. If the
original has named people, the asset stops at `needs_confirm` before anything
is uploaded, and the names are logged. Rerun with `--allow-face-loss` to
proceed, then reassign those people in Immich.

**Failures after upload.** The new asset is moved to the trash, the original
is left untouched, and the asset is marked `failed` with the error. Rerun with
`--retry-failed` to try again.

Logs: a JSON log goes to `<backup_dir>/immich-transcode.log` (`log_file` to
change). The console prints one line per asset and a total at the end.

### Decisions and deviations from the original spec

- **Asset update endpoint.** v3.2.4 has no non-deprecated replacement for
  `PUT /api/assets/{id}`. Its deprecation notice points back to itself, and
  the v3.3.0 release candidates don't add one either. Favorite and
  visibility are therefore sent with the upload. The update endpoint is only
  a fallback for description, rating, date and location, which no other
  endpoint can write. In testing these usually arrived through the file
  metadata and the copied sidecar, and no update call was made.
- **Output container and filename.** `.mov` sources stay MOV (iPhone files
  keep their name); everything else becomes MP4 with a `.mp4` extension.
  Audio is copied, except codecs MP4 can't carry (such as PCM from an MKV),
  which are re-encoded to AAC.
- **Rotation.** ffmpeg applies the display matrix to the pixels, so exiftool
  is told not to copy `Rotation` back, which would rotate the video twice.
  Validation checks the displayed size and orientation.
- **GPS precision.** ffmpeg writes a 3GPP `loci` box whose fixed-point
  coordinates are off by about 1 m, and exiftool's composite GPS (which Immich
  reads) prefers it. The box is removed unless the original had one.
- **Waiting for metadata extraction.** On v3.2.4, `hasMetadata` is already
  true right after upload, before the time zone is set. The tool waits until
  duration and dimensions appear, because they arrive in the same pass.
  Without this the timeline check fails for any video whose time zone comes
  from GPS.
- **Extra exclusions.** Live-photo motion parts (hidden visibility) are
  excluded: replacing them would break the link to the photo.
  External-library assets are excluded because Immich re-imports them from
  disk. Edited assets are excluded because their edits wouldn't carry over.
- **Concurrency** runs N full pipelines in parallel. The optional download
  prefetch was not built; with `concurrency > 1`, downloads overlap encodes
  anyway.

## Tests

```sh
make test   # unit tests; the pipeline tests also run ffmpeg/exiftool if installed
```

The unit tests cover bpp estimates and skip rules, ffmpeg arguments (including
the full-range case), probe parsing, the state machine and manifest store,
and config parsing. The pipeline tests generate sample clips and run the whole
state machine against an in-memory fake of Immich. They cover the dry run,
apply, rollback and retry, needs_confirm, low gain, HDR, corrupt input, an
interrupted encode and timeline mismatches.

Integration tests run against a **throwaway** Immich v3.2.4 in Docker, with
all data on tmpfs and the server on port 2299:

```sh
make immich-up && make integration && make immich-down
```

The test seeds a rotated phone portrait clip, a full-range Pixel-style clip,
an HDR clip, a corrupt file and a clip with a named face. It runs the real
binary (`scan`, a dry run, `run --apply`, `status`) and then checks the
server: the original is in the trash, and the replacement has the same
timeline date and location and keeps its album, favorite, description, rating
and tags.

A second test adds a partner user who shares their library with you and is
shown in your timeline. It first checks that Immich's search really does
return the partner's video. It then checks that `scan` leaves it out and that
`run --apply --asset <id>` refuses it. Finally it checks that the video was
not changed, recorded in the manifest, downloaded or copied into your
library.

The Immich client is generated with oapi-codegen (`make generate`), limited to
the operations listed in [`api/oapi-codegen.yaml`](api/oapi-codegen.yaml).

### The pinned API spec

`api/immich-openapi-specs.json` is Immich's own spec
(`open-api/immich-openapi-specs.json` in the immich repo), unmodified. Its
source is pinned in [`api/spec.mk`](api/spec.mk) by release tag, commit hash
and SHA-256:

```sh
make spec                                  # re-download the pinned spec and verify its checksum
make spec-update IMMICH_VERSION=v3.3.0     # pin a newer release
make generate && make test                 # then regenerate the client and test
```

`spec-update` resolves the tag to a commit, downloads the spec from that
commit and records its checksum. It also updates `SpecVersion` in the code and
the server image in the integration test's compose file. The Postgres and
Valkey images there are not updated; compare them with that release's
`docker-compose.yml`. A unit test fails if the spec file, `api/spec.mk`,
`SpecVersion` and the test server image ever disagree.

## Not in v1

- Per-file CRF search against a VMAF target (as in ab-av1's crf-search)
- Hardware encoders (VAAPI, QSV, NVENC)
- AV1 output
- HDR and 10-bit sources
