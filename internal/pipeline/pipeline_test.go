package pipeline

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"immich-transcode/internal/config"
	"immich-transcode/internal/immich"
	"immich-transcode/internal/immich/api"
	"immich-transcode/internal/logging"
	"immich-transcode/internal/manifest"
	"immich-transcode/internal/media"
	"immich-transcode/internal/testmedia"
)

const userID = "11111111-1111-4111-8111-111111111111"

// fakeImmich keeps assets in memory. Like the real server, it derives a new
// asset's date and location from the uploaded file's metadata.
type fakeImmich struct {
	t      *testing.T
	tools  media.Tools
	mu     sync.Mutex
	assets map[string]*immich.Asset
	files  map[string]string
	albums map[string][]string // asset → album names

	updates []api.UpdateAssetDto // every UpdateAsset call

	failCopy      bool
	shiftLocation bool // simulate the server reading a different location
	uploads       int
	downloads     int
}

func newFake(t *testing.T, tools media.Tools) *fakeImmich {
	return &fakeImmich{t: t, tools: tools, assets: map[string]*immich.Asset{}, files: map[string]string{}, albums: map[string][]string{}}
}

func (f *fakeImmich) add(path string, people ...string) string {
	id := uuid.NewString()
	a := f.describe(id, path)
	a.OriginalFileName = filepath.Base(path)
	var ppl []api.PersonResponseDto
	for _, p := range people {
		ppl = append(ppl, api.PersonResponseDto{Id: uuid.New(), Name: p})
	}
	a.People = &ppl
	desc := "family trip"
	a.ExifInfo.Description = &desc
	rating := 4
	a.ExifInfo.Rating = &rating
	a.IsFavorite = true
	a.Visibility = api.Archive
	tags := []api.TagResponseDto{{Id: uuid.New(), Name: "holiday", Value: "holiday"}}
	a.Tags = &tags
	f.assets[id] = a
	f.files[id] = path
	f.albums[id] = []string{"Summer"}
	return id
}

// describe builds an asset from a file, as metadata extraction would.
func (f *fakeImmich) describe(id, path string) *immich.Asset {
	f.t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatal(err)
	}
	sum := sha1.Sum(b)
	m, err := f.tools.ReadMeta(context.Background(), path)
	if err != nil {
		f.t.Fatal(err)
	}
	instant, _ := time.Parse("2006:01:02 15:04:05", m.CreateDate)
	// Immich derives a time zone from GPS; pretend every located clip was
	// shot at UTC-7, so localDateTime differs from the UTC instant.
	local := instant
	if m.GPSLatitude != nil {
		local = instant.Add(-7 * time.Hour)
	}
	size := len(b)
	dur := 3000
	w, h := 1280, 720
	a := &immich.Asset{
		Id:             uuid.MustParse(id),
		OwnerId:        uuid.MustParse(userID),
		Checksum:       base64.StdEncoding.EncodeToString(sum[:]),
		LocalDateTime:  local,
		FileCreatedAt:  instant,
		FileModifiedAt: instant,
		Duration:       &dur,
		Width:          &w,
		Height:         &h,
		Type:           api.VIDEO,
		Visibility:     api.Timeline,
		HasMetadata:    true,
		ExifInfo: &api.ExifResponseDto{
			FileSizeInByte:   &size,
			DateTimeOriginal: &instant,
			Latitude:         m.GPSLatitude,
			Longitude:        m.GPSLongitude,
		},
	}
	return a
}

func (f *fakeImmich) get(id string) (*immich.Asset, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.assets[id]
	if !ok {
		return nil, &immich.HTTPError{Op: "get", Status: 404}
	}
	cp := *a
	return &cp, nil
}

func (f *fakeImmich) GetAsset(_ context.Context, id string) (*immich.Asset, error) {
	return f.get(id)
}

func (f *fakeImmich) Download(_ context.Context, id string, w io.Writer, progress func(int64)) (string, int64, error) {
	f.mu.Lock()
	f.downloads++
	f.mu.Unlock()
	b, err := os.ReadFile(f.files[id])
	if err != nil {
		return "", 0, err
	}
	n, err := w.Write(b)
	progress(int64(n))
	sum := sha1.Sum(b)
	return base64.StdEncoding.EncodeToString(sum[:]), int64(n), err
}

func (f *fakeImmich) Upload(_ context.Context, u immich.UploadRequest, _ func(int64)) (immich.UploadResult, error) {
	b, err := os.ReadFile(u.Path)
	if err != nil {
		return immich.UploadResult{}, err
	}
	sum := sha1.Sum(b)
	cs := base64.StdEncoding.EncodeToString(sum[:])
	f.mu.Lock()
	for id, a := range f.assets {
		if a.Checksum == cs {
			f.mu.Unlock()
			return immich.UploadResult{ID: id, Duplicate: true}, nil
		}
	}
	f.mu.Unlock()
	// Keep our own copy: the pipeline deletes its work file when done.
	stored := filepath.Join(f.t.TempDir(), u.FileName)
	if err := os.WriteFile(stored, b, 0o644); err != nil {
		return immich.UploadResult{}, err
	}
	id := uuid.NewString()
	a := f.describe(id, stored)
	a.OriginalFileName = u.FileName
	a.FileCreatedAt, a.FileModifiedAt = u.FileCreatedAt, u.FileModifiedAt
	a.IsFavorite = u.IsFavorite
	if u.Visibility != "" {
		a.Visibility = u.Visibility
	}
	// Like Immich, report placeholders until extraction finishes: the
	// timeline date is the upload's fileCreatedAt and video facts are unset.
	local, dur, w, h := a.LocalDateTime, a.Duration, a.Width, a.Height
	a.LocalDateTime = u.FileCreatedAt
	a.Duration, a.Width, a.Height = nil, nil, nil
	if f.shiftLocation && a.ExifInfo.Latitude != nil {
		lat := *a.ExifInfo.Latitude + 1
		a.ExifInfo.Latitude = &lat
	}
	f.mu.Lock()
	f.assets[id] = a
	f.files[id] = stored
	f.uploads++
	f.mu.Unlock()
	go func() {
		time.Sleep(50 * time.Millisecond)
		f.mu.Lock()
		a.LocalDateTime, a.Duration, a.Width, a.Height = local, dur, w, h
		f.mu.Unlock()
	}()
	return immich.UploadResult{ID: id}, nil
}

func (f *fakeImmich) CopyRelationships(_ context.Context, from, to string) error {
	if f.failCopy {
		return errors.New("copy: HTTP 500")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.albums[to] = append([]string(nil), f.albums[from]...)
	f.assets[to].IsFavorite = f.assets[from].IsFavorite
	return nil
}

func (f *fakeImmich) UpdateAsset(_ context.Context, id string, dto api.UpdateAssetDto) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, dto)
	a := f.assets[id]
	if dto.DateTimeOriginal != nil {
		// Like Immich: the date is stored now, but localDateTime is only
		// recomputed by a background re-extraction.
		t, err := time.Parse("2006-01-02T15:04:05.000-07:00", *dto.DateTimeOriginal)
		if err != nil {
			return err
		}
		utc := t.UTC()
		a.ExifInfo.DateTimeOriginal = &utc
		local := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
		go func() {
			time.Sleep(50 * time.Millisecond)
			f.mu.Lock()
			a.LocalDateTime = local
			f.mu.Unlock()
		}()
	}
	if dto.Description != nil {
		a.ExifInfo.Description = dto.Description
	}
	if dto.Rating != nil {
		a.ExifInfo.Rating = dto.Rating
	}
	if dto.IsFavorite != nil {
		a.IsFavorite = *dto.IsFavorite
	}
	if dto.Visibility != nil {
		a.Visibility = *dto.Visibility
	}
	if dto.Latitude != nil {
		a.ExifInfo.Latitude, a.ExifInfo.Longitude = dto.Latitude, dto.Longitude
	}
	return nil
}

func (f *fakeImmich) TagAsset(_ context.Context, id string, tagIDs []openapi_types.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.assets[id]
	var tags []api.TagResponseDto
	if a.Tags != nil {
		tags = *a.Tags
	}
	for _, t := range tagIDs {
		tags = append(tags, api.TagResponseDto{Id: t})
	}
	a.Tags = &tags
	return nil
}

func (f *fakeImmich) Trash(_ context.Context, ids ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		f.assets[id].IsTrashed = true
	}
	return nil
}

func (f *fakeImmich) Restore(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.assets[id].IsTrashed = false
	return nil
}

func (f *fakeImmich) live() (n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.assets {
		if !a.IsTrashed {
			n++
		}
	}
	return n
}

var (
	fixturesOnce sync.Once
	fixtures     *testmedia.Set
	fixturesErr  error
)

type env struct {
	t     *testing.T
	fake  *fakeImmich
	store *manifest.Store
	proc  *Processor
	set   *testmedia.Set
}

func setup(t *testing.T) *env {
	t.Helper()
	for _, tool := range []string{"ffmpeg", "ffprobe", "exiftool"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not installed")
		}
	}
	fixturesOnce.Do(func() {
		dir, err := os.MkdirTemp("", "immich-transcode-fixtures")
		if err != nil {
			fixturesErr = err
			return
		}
		fixtures, fixturesErr = testmedia.Generate(dir, "ffmpeg", "exiftool")
	})
	if fixturesErr != nil {
		t.Fatal(fixturesErr)
	}
	tools := media.Tools{FFmpeg: "ffmpeg", FFprobe: "ffprobe", ExifTool: "exiftool"}
	dir := t.TempDir()
	cfg := config.Default()
	cfg.BackupDir = filepath.Join(dir, "backup")
	cfg.WorkDir = filepath.Join(dir, "work")
	cfg.Preset = "ultrafast"
	cfg.MetadataTimeout = 5 * time.Second
	if err := cfg.Finalize(); err != nil {
		t.Fatal(err)
	}
	store, err := manifest.Open(cfg.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	fake := newFake(t, tools)
	proc := &Processor{
		Cfg:           cfg,
		Opts:          Options{CRF: 28},
		Immich:        fake,
		Tools:         tools,
		Store:         store,
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		Console:       logging.NewConsole(),
		UserID:        userID,
		PollInterval:  10 * time.Millisecond,
		SettleTimeout: 500 * time.Millisecond,
	}
	return &env{t: t, fake: fake, store: store, proc: proc, set: fixtures}
}

func (e *env) state(id string) manifest.Record {
	e.t.Helper()
	r, ok, err := e.store.Get(context.Background(), id)
	if err != nil || !ok {
		e.t.Fatalf("no manifest row for %s: %v", id, err)
	}
	return r
}

func TestDryRunStopsBeforeUpload(t *testing.T) {
	e := setup(t)
	id := e.fake.add(e.set.Portrait.Path)
	o := e.proc.Process(context.Background(), id)
	if o.Err != nil || o.State != manifest.Validated {
		t.Fatalf("outcome %+v", o)
	}
	if e.fake.uploads != 0 || e.fake.assets[id].IsTrashed {
		t.Fatal("dry run touched the server")
	}
	r := e.state(id)
	if !exists(r.BackupPath) || !exists(r.OutputPath) {
		t.Fatal("backup or output missing")
	}
	// Applying later continues from the validated encode.
	e.proc.Opts.Apply = true
	o = e.proc.Process(context.Background(), id)
	if o.Err != nil || o.State != manifest.Done {
		t.Fatalf("apply outcome %+v", o)
	}
}

func TestApplyPortrait(t *testing.T) {
	e := setup(t)
	e.proc.Opts.Apply = true
	id := e.fake.add(e.set.Portrait.Path)
	o := e.proc.Process(context.Background(), id)
	if o.Err != nil || o.State != manifest.Done {
		t.Fatalf("outcome %+v", o)
	}
	r := e.state(id)
	if !e.fake.assets[id].IsTrashed {
		t.Error("original not trashed")
	}
	newA := e.fake.assets[r.NewAssetID]
	if newA == nil || newA.IsTrashed {
		t.Fatal("new asset missing")
	}
	if newA.OriginalFileName != "PXL_20210601_102030123.mp4" {
		t.Errorf("new filename %q", newA.OriginalFileName)
	}
	if !newA.LocalDateTime.Equal(e.fake.assets[id].LocalDateTime) {
		t.Errorf("date %v vs %v", newA.LocalDateTime, e.fake.assets[id].LocalDateTime)
	}
	if newA.Visibility != api.Archive || *newA.ExifInfo.Description != "family trip" || *newA.ExifInfo.Rating != 4 || len(*newA.Tags) != 1 {
		t.Errorf("patch incomplete: vis %s desc %v rating %v tags %v", newA.Visibility, newA.ExifInfo.Description, newA.ExifInfo.Rating, newA.Tags)
	}
	if len(e.fake.albums[r.NewAssetID]) != 1 {
		t.Error("albums not copied")
	}
	// Favorite and visibility travel with the upload; only fields with no
	// other API go through the deprecated update endpoint.
	for _, u := range e.fake.updates {
		if u.IsFavorite != nil || u.Visibility != nil {
			t.Errorf("update set favorite/visibility: %+v", u)
		}
	}
	if len(e.fake.updates) != 1 || e.fake.updates[0].Description == nil || e.fake.updates[0].Rating == nil {
		t.Errorf("expected one update with description and rating, got %+v", e.fake.updates)
	}
	if exists(r.OutputPath) {
		t.Error("work file not removed")
	}
	if !exists(r.BackupPath) {
		t.Error("backup removed")
	}
	if r.NewSize <= 0 || r.NewSize >= r.OriginalSize || r.CRF != 28 {
		t.Errorf("sizes %d → %d crf %d", r.OriginalSize, r.NewSize, r.CRF)
	}
	// Displayed orientation survives.
	pr, _ := e.proc.Tools.ProbeFile(context.Background(), e.fake.files[r.NewAssetID])
	if w, h := pr.Video.DisplaySize(); w >= h {
		t.Errorf("output displays %dx%d", w, h)
	}
	// Running again is a no-op.
	o = e.proc.Process(context.Background(), id)
	if o.State != manifest.Done || e.fake.uploads != 1 {
		t.Errorf("rerun: %+v uploads %d", o, e.fake.uploads)
	}
}

func TestFullRangeKeepsRange(t *testing.T) {
	e := setup(t)
	e.proc.Opts.Apply = true
	id := e.fake.add(e.set.FullRange.Path)
	o := e.proc.Process(context.Background(), id)
	if o.Err != nil || o.State != manifest.Done {
		t.Fatalf("outcome %+v", o)
	}
	r := e.state(id)
	pr, err := e.proc.Tools.ProbeFile(context.Background(), e.fake.files[r.NewAssetID])
	if err != nil {
		t.Fatal(err)
	}
	if pr.Video.ColorRange != "pc" || pr.Video.Codec != "hevc" {
		t.Errorf("output range %q codec %s", pr.Video.ColorRange, pr.Video.Codec)
	}
}

func TestSkips(t *testing.T) {
	e := setup(t)
	e.proc.Opts.Apply = true
	hdr := e.fake.add(e.set.HDR.Path)
	bad := e.fake.add(e.set.Corrupt.Path)
	if o := e.proc.Process(context.Background(), hdr); o.State != manifest.SkippedHDR {
		t.Errorf("hdr: %+v (%s)", o, e.state(hdr).Detail)
	}
	if o := e.proc.Process(context.Background(), bad); o.State != manifest.SkippedCorrupt {
		t.Errorf("corrupt: %+v", o)
	}
	if e.fake.uploads != 0 || e.fake.live() != 2 {
		t.Error("skipped assets touched the server")
	}
}

func TestLowGain(t *testing.T) {
	e := setup(t)
	e.proc.Opts.Apply = true
	e.proc.Cfg.MinSavingsPercent = 99.9
	id := e.fake.add(e.set.Portrait.Path)
	o := e.proc.Process(context.Background(), id)
	if o.State != manifest.SkippedLowGain {
		t.Fatalf("outcome %+v", o)
	}
	if exists(e.state(id).OutputPath) {
		t.Error("low-gain output not discarded")
	}
}

func TestNamedPeopleNeedConfirm(t *testing.T) {
	e := setup(t)
	e.proc.Opts.Apply = true
	id := e.fake.add(e.set.Portrait.Path, "Alice", "")
	o := e.proc.Process(context.Background(), id)
	if o.State != manifest.NeedsConfirm || e.fake.uploads != 0 {
		t.Fatalf("outcome %+v uploads %d", o, e.fake.uploads)
	}
	if r := e.state(id); r.People != "Alice" {
		t.Errorf("people = %q", r.People)
	}
	// Without the flag it stays put.
	if o := e.proc.Process(context.Background(), id); o.State != manifest.NeedsConfirm {
		t.Fatalf("reprocess: %+v", o)
	}
	e.proc.Opts.AllowFaceLoss = true
	if o := e.proc.Process(context.Background(), id); o.State != manifest.Done {
		t.Fatalf("allow-face-loss: %+v", o)
	}
}

func TestFailureAfterUploadRollsBack(t *testing.T) {
	e := setup(t)
	e.proc.Opts.Apply = true
	e.fake.failCopy = true
	id := e.fake.add(e.set.Portrait.Path)
	o := e.proc.Process(context.Background(), id)
	if o.State != manifest.Failed || o.Err == nil {
		t.Fatalf("outcome %+v", o)
	}
	r := e.state(id)
	if e.fake.assets[id].IsTrashed {
		t.Fatal("original trashed after failure")
	}
	if r.NewAssetID != "" || r.ResumeFrom != manifest.Validated || e.fake.live() != 1 {
		t.Fatalf("rollback incomplete: %+v live=%d", r, e.fake.live())
	}
	// Retry re-uploads; the server reports the trashed copy as a duplicate,
	// which is restored and used.
	e.fake.failCopy = false
	e.proc.Opts.RetryFailed = true
	o = e.proc.Process(context.Background(), id)
	if o.State != manifest.Done {
		t.Fatalf("retry: %+v", o)
	}
	if e.fake.uploads != 1 || e.fake.live() != 1 {
		t.Errorf("uploads %d live %d", e.fake.uploads, e.fake.live())
	}
}

func TestLocationDriftIsPatched(t *testing.T) {
	e := setup(t)
	e.proc.Opts.Apply = true
	e.fake.shiftLocation = true // server reads a different location from the new file
	id := e.fake.add(e.set.Portrait.Path)
	o := e.proc.Process(context.Background(), id)
	if o.State != manifest.Done {
		t.Fatalf("outcome %+v", o)
	}
	newA := e.fake.assets[e.state(id).NewAssetID]
	if !sameCoord(newA.ExifInfo.Latitude, e.fake.assets[id].ExifInfo.Latitude) {
		t.Errorf("latitude %v not patched", *newA.ExifInfo.Latitude)
	}
}

func TestTimelineMismatchStops(t *testing.T) {
	e := setup(t)
	e.proc.Opts.Apply = true
	// The original's timeline date differs from what the server derives
	// for the new file, and the patch cannot fix it (dateTimeOriginal
	// already matches), so verification must fail and roll back.
	id := e.fake.add(e.set.Portrait.Path)
	e.fake.assets[id].LocalDateTime = e.fake.assets[id].LocalDateTime.Add(3 * time.Hour)
	o := e.proc.Process(context.Background(), id)
	if o.State != manifest.Failed {
		t.Fatalf("outcome %+v", o)
	}
	if e.fake.assets[id].IsTrashed || e.fake.live() != 1 {
		t.Error("original trashed or new asset left behind")
	}
}

func TestInterruptedTranscodeIsClean(t *testing.T) {
	e := setup(t)
	id := e.fake.add(e.set.Portrait.Path)
	ctx, cancel := context.WithCancel(context.Background())
	// Advance to Probed, then cancel during the encode.
	for e.state2(id) != manifest.Probed {
		e.proc.stepOnce(ctx, id)
	}
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	o := e.proc.Process(ctx, id)
	if !o.Interrupted || o.State != manifest.Probed {
		t.Fatalf("outcome %+v", o)
	}
	matches, _ := filepath.Glob(filepath.Join(e.proc.Cfg.WorkDir, "*partial*"))
	if len(matches) != 0 {
		t.Errorf("partial files left: %v", matches)
	}
	if o := e.proc.Process(context.Background(), id); o.State != manifest.Validated {
		t.Errorf("resume: %+v", o)
	}
}

// state2 returns the state, or Pending when there is no row yet.
func (e *env) state2(id string) manifest.State {
	r, ok, _ := e.store.Get(context.Background(), id)
	if !ok {
		return manifest.Pending
	}
	return r.State
}

// stepOnce advances id by exactly one step (test helper).
func (p *Processor) stepOnce(ctx context.Context, id string) {
	a, _ := p.Immich.GetAsset(ctx, id)
	rec, _ := p.Store.Ensure(ctx, id, a.OriginalFileName, fileSize(a))
	to, msg, err := p.step(ctx, &rec, a)
	if err != nil {
		panic(err)
	}
	if err := p.Store.Transition(ctx, &rec, to, msg); err != nil {
		panic(err)
	}
}

func TestDiffAndFormat(t *testing.T) {
	inst := time.Date(2021, 6, 1, 10, 20, 30, 0, time.UTC)
	local := time.Date(2021, 6, 1, 3, 20, 30, 0, time.UTC) // UTC-7 wall time
	if got := FormatDateTimeOriginal(inst, local); got != "2021-06-01T03:20:30.000-07:00" {
		t.Errorf("format = %s", got)
	}
	lat, lon := 1.5, 2.5
	other := 9.0
	oldA := &immich.Asset{IsFavorite: true, Visibility: api.Archive, LocalDateTime: local,
		ExifInfo: &api.ExifResponseDto{DateTimeOriginal: &inst, Latitude: &lat, Longitude: &lon}}
	newA := &immich.Asset{Visibility: api.Timeline, LocalDateTime: local,
		ExifInfo: &api.ExifResponseDto{DateTimeOriginal: &inst, Latitude: &other, Longitude: &lon}}
	dto, fields := Diff(oldA, newA)
	if len(fields) != 3 || dto.IsFavorite == nil || dto.Visibility == nil || dto.Latitude == nil || dto.DateTimeOriginal != nil {
		t.Errorf("fields %v dto %+v", fields, dto)
	}
	if TimelineMismatch(oldA, newA) == "" {
		t.Error("location mismatch not detected")
	}
	newA.ExifInfo.Latitude = &lat
	if m := TimelineMismatch(oldA, newA); m != "" {
		t.Error(m)
	}
}

func TestOtherUsersAssetUntouched(t *testing.T) {
	e := setup(t)
	e.proc.Opts.Apply = true
	id := e.fake.add(e.set.Portrait.Path)
	e.fake.assets[id].OwnerId = uuid.New() // a partner's video
	o := e.proc.Process(context.Background(), id)
	if !errors.Is(o.Err, errNotOwner) {
		t.Fatalf("outcome %+v", o)
	}
	if _, ok, _ := e.store.Get(context.Background(), id); ok {
		t.Error("partner asset was recorded in the manifest")
	}
	if e.fake.uploads != 0 || e.fake.assets[id].IsTrashed {
		t.Error("partner asset touched")
	}
	entries, _ := os.ReadDir(e.proc.Cfg.BackupDir)
	for _, de := range entries {
		if !strings.HasPrefix(de.Name(), "manifest.db") {
			t.Errorf("backup dir has %s", de.Name())
		}
	}
}

func TestRepeatedDryRunReusesWork(t *testing.T) {
	e := setup(t)
	id := e.fake.add(e.set.Portrait.Path)
	ctx := context.Background()
	modTime := func() time.Time {
		fi, err := os.Stat(e.state(id).OutputPath)
		if err != nil {
			t.Fatal(err)
		}
		return fi.ModTime()
	}

	e.proc.Process(ctx, id) // crf 28 from setup
	first := modTime()

	// Same options again: nothing to do.
	if e.proc.Opts.HasWork(e.state(id)) {
		t.Error("HasWork true for a validated asset in a dry run")
	}
	e.proc.Process(ctx, id)
	if !modTime().Equal(first) || e.fake.downloads != 1 {
		t.Fatalf("second dry run redid work: downloads %d", e.fake.downloads)
	}

	// A different CRF from config (not --crf) keeps the existing encode.
	e.proc.Opts.CRF = 18
	e.proc.Process(ctx, id)
	if r := e.state(id); r.CRF != 28 || !modTime().Equal(first) {
		t.Fatalf("implicit crf change re-encoded: crf %d", r.CRF)
	}

	// An explicit --crf re-encodes from the backup without re-downloading.
	e.proc.Opts.CRFExplicit = true
	if !e.proc.Opts.HasWork(e.state(id)) {
		t.Error("HasWork false although --crf differs")
	}
	o := e.proc.Process(ctx, id)
	r := e.state(id)
	if o.State != manifest.Validated || r.CRF != 18 || modTime().Equal(first) || e.fake.downloads != 1 {
		t.Fatalf("explicit crf: state %s crf %d downloads %d", o.State, r.CRF, e.fake.downloads)
	}
	if r.NewSize <= 0 {
		t.Error("new size not updated")
	}
}

func TestHasWork(t *testing.T) {
	rec := func(s manifest.State, crf int) manifest.Record { return manifest.Record{State: s, CRF: crf} }
	cases := []struct {
		name string
		opts Options
		rec  manifest.Record
		want bool
	}{
		{"dry, validated", Options{CRF: 26}, rec(manifest.Validated, 26), false},
		{"dry, validated, config crf differs", Options{CRF: 20}, rec(manifest.Validated, 26), false},
		{"dry, validated, --crf differs", Options{CRF: 20, CRFExplicit: true}, rec(manifest.Validated, 26), true},
		{"dry, validated, --crf same", Options{CRF: 26, CRFExplicit: true}, rec(manifest.Validated, 26), false},
		{"apply, validated", Options{Apply: true, CRF: 26}, rec(manifest.Validated, 26), true},
		{"dry, probed", Options{CRF: 26}, rec(manifest.Probed, 0), true},
		{"dry, transcoded", Options{CRF: 26}, rec(manifest.Transcoded, 26), true},
	}
	for _, c := range cases {
		if got := c.opts.HasWork(c.rec); got != c.want {
			t.Errorf("%s: HasWork = %v, want %v", c.name, got, c.want)
		}
	}
	// Uploaded assets are never re-encoded, whatever --crf says.
	if (Options{CRF: 18, CRFExplicit: true}).Reencodes(rec(manifest.Uploaded, 26)) {
		t.Error("Reencodes true for an uploaded asset")
	}
}

func TestPixelVFRClipIsNotCorrupt(t *testing.T) {
	e := setup(t)
	e.proc.Opts.Apply = true
	id := e.fake.add(e.set.VFR.Path)
	o := e.proc.Process(context.Background(), id)
	if o.State != manifest.Done {
		t.Fatalf("outcome %+v (detail %q)", o, e.state(id).Detail)
	}
}

// TestDateEditedInImmichSurvives covers a video whose date was corrected in
// Immich: the file says 2017 but the timeline shows 2006. The patch must
// restore 2006, and verification must wait for Immich to apply it.
func TestDateEditedInImmichSurvives(t *testing.T) {
	e := setup(t)
	e.proc.Opts.Apply = true
	id := e.fake.add(e.set.Portrait.Path)
	orig := e.fake.assets[id]
	instant := time.Date(2006, 8, 4, 1, 21, 0, 0, time.UTC)
	orig.ExifInfo.DateTimeOriginal = &instant
	orig.LocalDateTime = time.Date(2006, 8, 3, 21, 21, 0, 0, time.UTC) // UTC-4 wall time

	o := e.proc.Process(context.Background(), id)
	if o.State != manifest.Done {
		t.Fatalf("outcome %+v", o)
	}
	newA := e.fake.assets[e.state(id).NewAssetID]
	if !newA.LocalDateTime.Equal(orig.LocalDateTime) {
		t.Errorf("localDateTime %v, want %v", newA.LocalDateTime, orig.LocalDateTime)
	}
}
