# immich-transcode

**Shrink the videos in your Immich library without losing anything.**

> **Note:** this utility was written with an AI coding assistant rather than
> by hand. It has been tested on a real Immich library with tens
> of thousands of photos and videos, but review what it does before running
> it with `--apply` on your own library, and keep the backups it makes.

Phones record video at a fixed, high bitrate whatever is in the scene, so
videos often take up most of an Immich library's disk space. immich-transcode
finds the large, inefficiently encoded ones and replaces each with an HEVC
(x265) re-encode that aims for similar visual quality. On real phone videos the savings
ranged from about a third to over 90% per video, depending on the content.

Each video is replaced in place, as if it had always been the smaller file:

- **Nothing is lost from Immich.** The timeline date, location, albums,
  favorites, description, rating, tags, archive status, shared links and
  stacks carry over to the replacement.
- **Every original is backed up** locally and checked against Immich's
  checksum before anything changes. Originals are only moved to the Immich
  trash, never deleted.
- **Every re-encode is checked before upload**: it must decode cleanly and
  match the original's duration, frame count, orientation, date and GPS.
- **Dry run by default.** Nothing is uploaded or trashed without `--apply`.
- **Undoable.** `verify` re-checks finished replacements, `redo` re-encodes
  one from its backup, and `restore` puts the original back.

Works with Immich v3 (tested on v3.2.4 and v3.3.1). Used on Linux; builds
are also provided for macOS and Windows. It's an independent community tool,
not part of the Immich project.

## Requirements

- Immich server **v3.0 or later**. The client is generated from the v3.3.1 API
  spec and tested against v3.2.4 and v3.3.1.
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

## Install

Download the binary for your system from the
[releases page](https://github.com/dvanbler/immich-transcode/releases), check
it against `SHA256SUMS` (`sha256sum -c SHA256SUMS --ignore-missing`), make it
executable and put it on your PATH. `immich-transcode --version` shows which
release you have.

Or build from source (Go, no cgo):

```sh
make build        # ./immich-transcode
make dist         # all platforms, plus SHA256SUMS, in dist/
```

The version is stamped in from `git describe`: the tag on a release, e.g.
`v0.1.0-3-gabc1234-dirty` between releases.

## Configuration

Settings come from a YAML config file, with command-line flags taking
precedence. Start from the example in this repository.

**1. Copy the example config** to the default location for your system:

```sh
# Linux
mkdir -p ~/.config/immich-transcode
cp config.example.yaml ~/.config/immich-transcode/config.yaml

# macOS
mkdir -p ~/Library/Application\ Support/immich-transcode
cp config.example.yaml ~/Library/Application\ Support/immich-transcode/config.yaml
```

On Windows the location is `%AppData%\immich-transcode\config.yaml`. A config
file anywhere else works too: pass it with `--config <path>` on every command.

**2. Edit it.** Two settings are required:

- `server_url`: your Immich address, e.g. `https://photos.example.com`.
- `backup_dir`: where the originals are backed up. The originals are kept,
  never deleted, so this needs room for the full size of every video you
  process. Use a disk that is itself backed up. The manifest, the log, and the
  encodes waiting for upload (`work_dir`) go here too, unless you set them
  separately.

Everything else has a sensible default and is explained in the file. The ones
most worth a look:

- `taken_before`: skip videos that may still be on a phone (see
  [Videos still on a phone](#videos-still-on-a-phone)).
- `preset` and `crf`: encoding speed versus file size and quality. In
  testing, `medium` was about 2.5× faster than the default `slow` at a similar
  file size.
- `min_size`: ignore videos smaller than this.

Unknown keys are rejected, so a typo in a setting name is reported rather than
silently ignored.

**3. Set the API key.** It is read only from the `IMMICH_API_KEY` environment
variable, never from the config file. Create it in Immich under Account
Settings → API Keys, with the permissions listed under
[Requirements](#requirements). To keep it out of your shell history, you can
store it in a file only you can read:

```sh
echo 'your-key' > ~/.config/immich-transcode/api-key
chmod 600 ~/.config/immich-transcode/api-key
export IMMICH_API_KEY=$(cat ~/.config/immich-transcode/api-key)
```

**4. Check it.** `immich-transcode config` shows the settings the tool will
use, which config file it read, and whether the API key is set (without
showing it). `immich-transcode scan` then confirms the server address,
server version and the read permissions without changing anything. The
extra permissions for `--apply` are checked when you first run with
`--apply`.

**Several Immich users:** the tool processes only the videos of the user who
owns the API key. For another user, make a second config file with its own
`backup_dir`, so each user gets a separate manifest, and run with that user's
key:

```sh
cp ~/.config/immich-transcode/config.yaml ~/.config/immich-transcode/partner.yaml
# edit backup_dir (and taken_before) in partner.yaml
IMMICH_API_KEY=$(cat ~/.config/immich-transcode/partner-key) \
  immich-transcode --config ~/.config/immich-transcode/partner.yaml scan
```

## Usage

After [configuring](#configuration) the tool:

```sh
immich-transcode scan                      # ranked candidates, read-only
immich-transcode run --limit 3             # dry run: download, encode, validate
immich-transcode status                    # manifest: state, savings, failures
immich-transcode resume --apply            # replace exactly those validated videos in Immich
immich-transcode reset --state skipped_corrupt   # re-check skipped assets
immich-transcode verify                    # re-check done replacements as stored in Immich
immich-transcode redo --asset <id> --apply # re-encode one from its backup and replace it
immich-transcode restore --asset <id>      # put the original back (add --apply)
```

`run` searches Immich for candidates and picks the best by estimated
savings, starting new videos as well as continuing unfinished ones. `resume`
only continues what the manifest already has unfinished: assets partway
through, dry-run encodes awaiting `--apply`, and `pending` assets such as
those sent back by `reset`. It never starts new videos. To commit exactly
what a dry run validated, use `resume --apply`.

`immich-transcode config` prints the effective settings (defaults, then the
config file, then any flags) as config-file YAML, and says whether
`IMMICH_API_KEY` is set without showing it.

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

### Videos still on a phone

The tool never touches phones, but a phone that still has a video can
upload it again. Once the replaced original is emptied from the Immich trash,
the Immich app no longer sees the phone's copy as backed up, so auto-backup
uploads it again: you end up with both versions in Immich and lose the
saving.

To avoid that, only transcode videos the phones no longer have:

1. In the Immich app, use "Free up space" first, while the originals are
   still on the server (afterwards the app no longer counts them as backed
   up and won't remove them).
2. Set `taken_before` to that date, in the config file or with
   `--taken-before 2026-09-01` on `scan` and `run`. An age such as `90d`
   also works. Explicit `--asset` ids are not limited by it.

Immich filters by the date a video was taken, not when it was uploaded.
`scan` and `run` print the cutoff in use.

### Checking finished replacements

`verify` downloads each `done` replacement from Immich and runs the
pre-upload validation on it again, against your local backup of the
original. That covers a clean decode, duration, displayed size and
orientation, audio streams, frame count, duplicate timestamps, creation
date and GPS. It's read-only towards Immich, and each download is deleted
after checking. It lists any replacements with problems and exits non-zero
if there are any. Use `--asset <id>` to check specific assets.

### Redoing a replacement

`redo --asset <id>` re-encodes a `done` asset from the local backup of its
original, and replaces the replacement currently in Immich, for example one
that `verify` reported. The current replacement is treated like an original:
its albums, favorite, shared links, stack, description, rating, tags and
date carry over to the new upload, including anything you changed after the
first replacement. The new upload is verified before the old replacement is
moved to the trash. Nothing is downloaded; if the backup is missing, the
redo refuses rather than re-encoding the replacement. Without `--apply` it
stops after validating the new encode, like `run`.

If the fresh encode is byte-identical to the current replacement (same
ffmpeg and settings), nothing is uploaded and the current replacement is
kept. Replacements made by this tool are never offered as candidates by
`scan` or `run`.

### Putting an original back

`restore --asset <id>` puts the backup of a `done` asset's original back in
Immich, in place of its replacement. Without `--apply` it only shows what
would happen.

If the original is still in the Immich trash (30 days by default), it is
restored from there, including its face assignments. Otherwise the backup is
uploaded as a new asset, without face assignments. Either way, albums,
favorite, shared links, stack, description, rating, tags and date are copied
from the current replacement. The timeline is checked before the
replacement is moved to the trash. The backup file is never moved or
deleted, and restored assets end in the `restored` state, so `scan` and
`run` don't select them again.

### Re-checking skipped assets

Skipped assets are never selected again. Use `reset` to send them back to
`pending`, for example after changing the `bpp` thresholds or after a fix to a
check. It changes only the manifest: the next `resume` or `run` verifies the
existing backup by checksum instead of downloading it, then re-runs every
check.
`reset` also accepts `failed` and specific `--asset` ids. Assets that are
`done` can't be reset.

### Fixing a failed asset

First see why it failed:

```sh
immich-transcode status --state failed
```

The DETAIL column shows the error; `<backup_dir>/immich-transcode.log` has
the full story. Then pick one of two fixes.

**Retry from where it stopped**, for temporary problems: network errors,
server timeouts, Ctrl-C, or the timeline check not settling in time.

```sh
immich-transcode resume --retry-failed           # dry run
immich-transcode resume --retry-failed --apply   # finish it
```

This continues from the last step that succeeded, reusing the backup and the
encode. If the failure came after the upload, the new asset has already been
moved to the trash and the original kept, so the retry uploads again. Immich
recognizes the same file in the trash and the tool restores it rather than
creating a copy.

**Start over**, when the encode itself was bad, i.e. the error mentions
validation: "does not decode cleanly", "share a timestamp", "packet count",
"duration" and similar. A retry would re-check the same bad file and fail
again.

```sh
immich-transcode reset --asset <id>
immich-transcode resume --apply
```

`reset` sends the asset back to `pending`. The next run verifies the
existing backup by checksum (no download) and encodes again from scratch.

**If the error says "ROLLBACK FAILED"**, the tool couldn't move the new asset
to the trash after the failure, so it's still in the library next to the
original. Check that asset in Immich (the error names it) and trash it by
hand before retrying.

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
is left untouched, and the asset is marked `failed` with the error. See
[Fixing a failed asset](#fixing-a-failed-asset).

Logs: a JSON log goes to `<backup_dir>/immich-transcode.log` (`log_file` to
change). The console prints one line per asset and a total at the end.

### Decisions and deviations from the original spec

- **Asset update endpoint.** Neither v3.2.4 nor v3.3.1 has a non-deprecated
  replacement for `PUT /api/assets/{id}`; its deprecation notice points back
  to itself. Favorite and
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

Integration tests run against a **throwaway** Immich in Docker (the version
pinned in `api/spec.mk`, currently v3.3.1), with all data on tmpfs and the
server on port 2299:

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
make spec-update IMMICH_VERSION=v3.4.0     # pin a newer release
make generate && make test                 # then regenerate the client and test
```

`spec-update` resolves the tag to a commit, downloads the spec from that
commit and records its checksum. It also updates `SpecVersion` in the code and
the server image in the integration test's compose file. The Postgres and
Valkey images there are not updated; compare them with that release's
`docker-compose.yml`. A unit test fails if the spec file, `api/spec.mk`,
`SpecVersion` and the test server image ever disagree.

## Making a release

Push a version tag; GitHub Actions does the rest
([.github/workflows/release.yml](.github/workflows/release.yml)):

```sh
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

The workflow runs the unit tests and the integration tests against a
throwaway Immich, builds the binaries for every platform with the tag as their
version, and publishes a GitHub release with the binaries, `SHA256SUMS` and
generated notes. If any test fails, nothing is published. Tags with a hyphen
(`v0.2.0-rc.1`) become pre-releases.

## Not in v1

- Per-file CRF search against a VMAF target (as in ab-av1's crf-search)
- Hardware encoders (VAAPI, QSV, NVENC)
- AV1 output
- HDR and 10-bit sources
