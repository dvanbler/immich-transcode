//go:build integration

// Package integration runs immich-transcode end to end against a throwaway
// Immich v3 server (see docker-compose.yml in this directory).
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"immich-transcode/internal/immich"
	"immich-transcode/internal/manifest"
	"immich-transcode/internal/testmedia"
)

func serverURL() string {
	if u := os.Getenv("IMMICH_TEST_URL"); u != "" {
		return u
	}
	return "http://127.0.0.1:2299"
}

// rawAPI is a small JSON client for setup calls the tool itself never makes.
type rawAPI struct {
	t      *testing.T
	base   string
	header http.Header
}

func (r *rawAPI) do(method, path string, in, out any) int {
	r.t.Helper()
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, r.base+"/api"+path, body)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range r.header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		r.t.Logf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, b)
	} else if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			r.t.Fatalf("%s %s: decode: %v: %s", method, path, err, b)
		}
	}
	return resp.StatusCode
}

func waitForServer(t *testing.T, base string) {
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/api/server/ping")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("Immich at %s did not become ready; start it with docker compose -f test/integration/docker-compose.yml up -d", base)
}

// adminKey signs up the admin (first run) or logs in, and returns an API key
// with all permissions and the admin's user id.
func adminKey(t *testing.T, base string) (key, userID string) {
	anon := &rawAPI{t: t, base: base}
	anon.do("POST", "/auth/admin-sign-up", map[string]string{
		"email": "admin@example.test", "password": "integration-test", "name": "Admin"}, nil) // 400 if it exists
	return userKey(t, base, "admin@example.test", "integration-test")
}

// userKey logs in and creates an API key with all permissions.
func userKey(t *testing.T, base, email, password string) (key, userID string) {
	anon := &rawAPI{t: t, base: base}
	var login struct{ AccessToken, UserID string }
	if code := anon.do("POST", "/auth/login", map[string]string{"email": email, "password": password}, &login); code != 201 {
		t.Fatalf("login %s failed: %d", email, code)
	}
	user := &rawAPI{t: t, base: base, header: http.Header{"Authorization": {"Bearer " + login.AccessToken}}}
	var k struct{ Secret string }
	if code := user.do("POST", "/api-keys", map[string]any{"name": "integration " + time.Now().Format(time.RFC3339Nano), "permissions": []string{"all"}}, &k); code != 201 {
		t.Fatalf("create api key for %s: %d", email, code)
	}
	return k.Secret, login.UserID
}

// tool runs the immich-transcode binary with its own config, manifest and
// backup directory.
type tool struct {
	t       *testing.T
	bin     string
	cfgPath string
	backup  string
	key     string
}

func newTool(t *testing.T, base, key, work string) *tool {
	tl := &tool{t: t, bin: buildBinary(t), cfgPath: filepath.Join(work, "config.yaml"),
		backup: filepath.Join(work, "backup"), key: key}
	os.WriteFile(tl.cfgPath, []byte(fmt.Sprintf("server_url: %s\nbackup_dir: %s\nwork_dir: %s\npreset: ultrafast\nmin_size: 1MB\nmetadata_timeout: 2m\n",
		base, tl.backup, filepath.Join(work, "tmp"))), 0o644)
	return tl
}

// run executes the tool and returns its combined output and exit error.
func (tl *tool) run(args ...string) (string, error) {
	tl.t.Helper()
	cmd := exec.Command(tl.bin, append([]string{"--config", tl.cfgPath}, args...)...)
	cmd.Env = append(os.Environ(), "IMMICH_API_KEY="+tl.key)
	out, err := cmd.CombinedOutput()
	tl.t.Logf("$ immich-transcode %s\n%s", strings.Join(args, " "), out)
	return string(out), err
}

func TestEndToEnd(t *testing.T) {
	base := serverURL()
	waitForServer(t, base)
	key, _ := adminKey(t, base)
	api := &rawAPI{t: t, base: base, header: http.Header{"X-Api-Key": {key}}}
	client, err := immich.New(base, key)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	work := t.TempDir()

	set, err := testmedia.Generate(work, "ffmpeg", "exiftool")
	if err != nil {
		t.Fatal(err)
	}
	// A second portrait clip whose original has a named person on it.
	peoplePath := filepath.Join(work, "PXL_20210602_090000000.mp4")
	copyFile(t, set.Portrait.Path, peoplePath)
	// Stamp every clip so its checksum is new to the server even when the
	// test is rerun against the same instance.
	stamp := fmt.Sprintf("run %d", time.Now().UnixNano())
	for i, p := range []string{set.Portrait.Path, set.FullRange.Path, set.HDR.Path, set.Corrupt.Path, peoplePath} {
		out, err := exec.Command("exiftool", "-overwrite_original", fmt.Sprintf("-Keys:Comment=%s/%d", stamp, i), p).CombinedOutput()
		if err != nil {
			t.Fatalf("exiftool stamp %s: %v %s", p, err, out)
		}
	}

	upload := func(path string) string {
		fi, _ := os.Stat(path)
		hexSum, _, _ := immich.FileSHA1(path)
		res, err := client.Upload(ctx, immich.UploadRequest{
			Path: path, FileName: filepath.Base(path),
			FileCreatedAt: fi.ModTime(), FileModifiedAt: fi.ModTime(), SHA1Hex: hexSum,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return res.ID
	}
	ids := map[string]string{
		"portrait":  upload(set.Portrait.Path),
		"fullrange": upload(set.FullRange.Path),
		"hdr":       upload(set.HDR.Path),
		"corrupt":   upload(set.Corrupt.Path),
		"people":    upload(peoplePath),
	}
	for name, id := range ids {
		waitMetadata(t, client, id, name)
	}

	// Decorate the portrait original with everything that must survive.
	var album struct{ ID string }
	api.do("POST", "/albums", map[string]any{"albumName": "Trip", "assetIds": []string{ids["portrait"]}}, &album)
	desc, rating := "harbour at dusk", 4
	api.do("PUT", "/assets/"+ids["portrait"], map[string]any{"isFavorite": true, "description": desc, "rating": rating, "visibility": "archive"}, nil)
	var tags []struct{ ID string }
	api.do("PUT", "/tags", map[string]any{"tags": []string{"Travel/Seattle"}}, &tags)
	api.do("PUT", "/tags/assets", map[string]any{"assetIds": []string{ids["portrait"]}, "tagIds": []string{tags[len(tags)-1].ID}}, nil)

	var person struct{ ID string }
	api.do("POST", "/people", map[string]any{"name": "Alice"}, &person)
	api.do("POST", "/faces", map[string]any{"assetId": ids["people"], "personId": person.ID,
		"imageWidth": 720, "imageHeight": 1280, "x": 100, "y": 100, "width": 200, "height": 200}, nil)

	before, _ := client.GetAsset(ctx, ids["portrait"])

	tl := newTool(t, base, key, work)
	cli := func(args ...string) string {
		out, err := tl.run(args...)
		// Expected: some fixtures end failed/skipped, which exits non-zero.
		if err != nil && !strings.Contains(out, "had errors") {
			t.Fatalf("immich-transcode %v: %v", args, err)
		}
		return out
	}
	backup := tl.backup

	scan := cli("scan", "--limit", "0")
	for _, name := range []string{"portrait", "fullrange", "people"} {
		if !strings.Contains(scan, ids[name]) {
			t.Errorf("scan did not list %s (%s)", name, ids[name])
		}
	}

	// Dry run: nothing changes on the server.
	cli("run", "--asset", ids["portrait"])
	// A second dry run has nothing left to do for it.
	if again := cli("run", "--asset", ids["portrait"]); !strings.Contains(again, "already validated") {
		t.Error("repeated dry run did not skip the validated asset")
	}
	// Consecutive limited dry runs work through new candidates.
	batch1 := cli("run", "--limit", "1")
	batch2 := cli("run", "--limit", "1")
	full := filepath.Base(set.FullRange.Path) // largest estimated saving
	if !strings.Contains(batch1, full+":") || strings.Contains(batch2, full+":") {
		t.Error("second --limit 1 dry run did not move past the first batch")
	}
	if a, _ := client.GetAsset(ctx, ids["portrait"]); a.IsTrashed {
		t.Fatal("dry run trashed the original")
	}

	var assetArgs []string
	for _, id := range ids {
		assetArgs = append(assetArgs, "--asset", id)
	}
	cli(append([]string{"run", "--apply"}, assetArgs...)...)
	status := cli("status", "--all")

	store, err := manifest.Open(filepath.Join(backup, "manifest.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	want := map[string]manifest.State{
		"portrait":  manifest.Done,
		"fullrange": manifest.Done,
		"hdr":       manifest.SkippedHDR,
		"corrupt":   manifest.SkippedCorrupt,
		"people":    manifest.NeedsConfirm,
	}
	recs := map[string]manifest.Record{}
	for name, id := range ids {
		r, ok, _ := store.Get(ctx, id)
		recs[name] = r
		if !ok || r.State != want[name] {
			t.Errorf("%s: state %q, want %q (error %q detail %q)", name, r.State, want[name], r.Error, r.Detail)
		}
	}
	if !strings.Contains(status, "Total saved") {
		t.Error("status output missing total")
	}

	// Portrait: the replacement carries everything over.
	r := recs["portrait"]
	if r.State == manifest.Done {
		oldA, _ := client.GetAsset(ctx, ids["portrait"])
		if !oldA.IsTrashed {
			t.Error("original not in trash")
		}
		newA, err := client.GetAsset(ctx, r.NewAssetID)
		if err != nil {
			t.Fatal(err)
		}
		if !newA.LocalDateTime.Equal(before.LocalDateTime) {
			t.Errorf("localDateTime %v, want %v", newA.LocalDateTime, before.LocalDateTime)
		}
		if lat := newA.ExifInfo.Latitude; lat == nil || *lat-set.Portrait.Lat > 1e-4 || set.Portrait.Lat-*lat > 1e-4 {
			t.Errorf("latitude %v, want %v", lat, set.Portrait.Lat)
		}
		if !newA.IsFavorite {
			t.Error("favorite lost")
		}
		if newA.Visibility != "archive" {
			t.Errorf("visibility %q, want archive", newA.Visibility)
		}
		if newA.ExifInfo.Description == nil || *newA.ExifInfo.Description != desc {
			t.Errorf("description %v", newA.ExifInfo.Description)
		}
		if newA.ExifInfo.Rating == nil || *newA.ExifInfo.Rating != rating {
			t.Errorf("rating %v", newA.ExifInfo.Rating)
		}
		if newA.Tags == nil || len(*newA.Tags) == 0 {
			t.Error("tags lost")
		}
		if newA.OriginalFileName != filepath.Base(set.Portrait.Path) {
			t.Errorf("filename %q", newA.OriginalFileName)
		}
		// v3 album responses no longer list assets; search by album instead.
		var inAlbum struct {
			Assets struct{ Items []struct{ ID string } }
		}
		api.do("POST", "/search/metadata", map[string]any{"filter": map[string]any{"albumIds": map[string]any{"any": []string{album.ID}}}}, &inAlbum)
		found := false
		for _, a := range inAlbum.Assets.Items {
			found = found || a.ID == r.NewAssetID
		}
		if !found {
			t.Error("new asset not in album")
		}
		if fi, err := os.Stat(r.BackupPath); err != nil || fi.Size() != r.OriginalSize {
			t.Errorf("backup %s missing or wrong size", r.BackupPath)
		}
	}
	for _, name := range []string{"hdr", "corrupt", "people"} {
		if a, _ := client.GetAsset(ctx, ids[name]); a == nil || a.IsTrashed {
			t.Errorf("%s original was trashed", name)
		}
	}
	if !strings.Contains(recs["people"].People, "Alice") {
		t.Errorf("people not recorded: %q", recs["people"].People)
	}
}

func waitMetadata(t *testing.T, c *immich.Client, id, name string) {
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		a, err := c.GetAsset(context.Background(), id)
		if err == nil && a.HasMetadata {
			return
		}
		time.Sleep(time.Second)
	}
	t.Logf("%s: metadata extraction did not finish", name)
}

func buildBinary(t *testing.T) string {
	bin := filepath.Join(t.TempDir(), "immich-transcode")
	cmd := exec.Command("go", "build", "-o", bin, "immich-transcode")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	return bin
}

func copyFile(t *testing.T, from, to string) {
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestPartnerVideosUntouched checks that videos owned by a partner, which
// Immich's search returns when the partner is shown in the timeline, are
// never listed, recorded, downloaded or changed.
func TestPartnerVideosUntouched(t *testing.T) {
	base := serverURL()
	waitForServer(t, base)
	ctx := context.Background()
	work := t.TempDir()
	adminK, adminID := adminKey(t, base)
	admin := &rawAPI{t: t, base: base, header: http.Header{"X-Api-Key": {adminK}}}

	// A fresh partner user each run, so reruns never see duplicates.
	email := fmt.Sprintf("partner-%d@example.test", time.Now().UnixNano())
	if code := admin.do("POST", "/admin/users", map[string]any{"email": email, "password": "partner-pass", "name": "Partner"}, nil); code != 201 {
		t.Fatalf("create partner user: %d", code)
	}
	partnerK, partnerID := userKey(t, base, email, "partner-pass")
	partner := &rawAPI{t: t, base: base, header: http.Header{"X-Api-Key": {partnerK}}}
	partnerClient, err := immich.New(base, partnerK)
	if err != nil {
		t.Fatal(err)
	}

	// The partner owns a large, inefficient video: a prime candidate.
	// Unique name so the "no copy appeared" check below ignores other runs.
	clipName := fmt.Sprintf("PARTNER_CLIP_%d", time.Now().UnixNano())
	clip := filepath.Join(work, clipName+".mp4")
	if out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30,noise=alls=40:allf=t", "-t", "3",
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "8", "-pix_fmt", "yuv420p", clip).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	fi, _ := os.Stat(clip)
	hexSum, b64Sum, _ := immich.FileSHA1(clip)
	res, err := partnerClient.Upload(ctx, immich.UploadRequest{Path: clip, FileName: filepath.Base(clip),
		FileCreatedAt: fi.ModTime(), FileModifiedAt: fi.ModTime(), SHA1Hex: hexSum}, nil)
	if err != nil {
		t.Fatal(err)
	}
	partnerAsset := res.ID
	waitMetadata(t, partnerClient, partnerAsset, "partner clip")

	// Partner shares with the admin; the admin shows them in the timeline.
	if code := partner.do("POST", "/partners", map[string]any{"sharedWithId": adminID}, nil); code != 201 {
		t.Fatalf("create partner share: %d", code)
	}
	if code := admin.do("PUT", "/partners/"+partnerID, map[string]any{"inTimeline": true}, nil); code != 200 {
		t.Fatalf("show partner in timeline: %d", code)
	}

	// Precondition: the admin's search really does return the partner's
	// video. Without this the rest of the test would prove nothing.
	adminClient, err := immich.New(base, adminK)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	if err := adminClient.SearchVideos(ctx, 1_000_000, func(page []immich.Asset) error {
		for _, a := range page {
			seen = seen || a.Id.String() == partnerAsset
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("precondition failed: the admin's search does not return the partner's video")
	}

	tl := newTool(t, base, adminK, work)

	scan, err := tl.run("scan", "--limit", "0")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if strings.Contains(scan, partnerAsset) {
		t.Error("scan listed the partner's video")
	}
	if !strings.Contains(scan, "partner asset (not owned)") {
		t.Error("scan did not report excluding partner assets")
	}

	// Asking for it explicitly must be refused before anything happens.
	out, err := tl.run("run", "--apply", "--asset", partnerAsset)
	if err == nil {
		t.Error("run --asset on a partner video exited 0")
	}
	if !strings.Contains(out, "belongs to another user") {
		t.Error("run did not explain the refusal")
	}

	// Nothing changed on the server or locally.
	a, err := partnerClient.GetAsset(ctx, partnerAsset)
	if err != nil {
		t.Fatal(err)
	}
	if a.IsTrashed || a.Checksum != b64Sum {
		t.Errorf("partner video changed: trashed=%v checksum %s (was %s)", a.IsTrashed, a.Checksum, b64Sum)
	}
	store, err := manifest.Open(filepath.Join(tl.backup, "manifest.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, ok, _ := store.Get(ctx, partnerAsset); ok {
		t.Error("partner video recorded in the manifest")
	}
	filepath.WalkDir(tl.backup, func(path string, d os.DirEntry, _ error) error {
		if d != nil && !d.IsDir() && strings.Contains(d.Name(), partnerAsset) {
			t.Errorf("partner video downloaded to %s", path)
		}
		return nil
	})
	// And no copy of it appeared in the admin's own library.
	var dup struct {
		Assets struct {
			Items []struct{ ID, OwnerID string }
		}
	}
	admin.do("POST", "/search/metadata", map[string]any{"filter": map[string]any{
		"originalFileName": map[string]any{"like": clipName + "%"}}}, &dup)
	for _, d := range dup.Assets.Items {
		if d.OwnerID == adminID {
			t.Errorf("admin now owns a copy of the partner video: %s", d.ID)
		}
	}
}
