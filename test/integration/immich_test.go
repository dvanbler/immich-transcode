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

// apiKey signs up the admin (first run) or logs in, and creates a key with
// all permissions.
func apiKey(t *testing.T, base string) string {
	anon := &rawAPI{t: t, base: base}
	creds := map[string]string{"email": "admin@example.test", "password": "integration-test", "name": "Admin"}
	anon.do("POST", "/auth/admin-sign-up", creds, nil) // 400 if it already exists
	var login struct{ AccessToken string }
	if code := anon.do("POST", "/auth/login", creds, &login); code != 201 {
		t.Fatalf("login failed: %d", code)
	}
	user := &rawAPI{t: t, base: base, header: http.Header{"Authorization": {"Bearer " + login.AccessToken}}}
	var key struct{ Secret string }
	if code := user.do("POST", "/api-keys", map[string]any{"name": "integration " + time.Now().Format(time.RFC3339Nano), "permissions": []string{"all"}}, &key); code != 201 {
		t.Fatalf("create api key: %d", code)
	}
	return key.Secret
}

func TestEndToEnd(t *testing.T) {
	base := serverURL()
	waitForServer(t, base)
	key := apiKey(t, base)
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

	bin := buildBinary(t)
	cfgPath := filepath.Join(work, "config.yaml")
	backup := filepath.Join(work, "backup")
	os.WriteFile(cfgPath, []byte(fmt.Sprintf("server_url: %s\nbackup_dir: %s\nwork_dir: %s\npreset: ultrafast\nmin_size: 1MB\nmetadata_timeout: 2m\n",
		base, backup, filepath.Join(work, "tmp"))), 0o644)
	cli := func(args ...string) string {
		cmd := exec.Command(bin, append([]string{"--config", cfgPath}, args...)...)
		cmd.Env = append(os.Environ(), "IMMICH_API_KEY="+key)
		out, err := cmd.CombinedOutput()
		t.Logf("$ immich-transcode %s\n%s", strings.Join(args, " "), out)
		if err != nil && !strings.Contains(string(out), "had errors") {
			t.Fatalf("immich-transcode %v: %v", args, err)
		}
		return string(out)
	}

	scan := cli("scan", "--limit", "0")
	for _, name := range []string{"portrait", "fullrange", "people"} {
		if !strings.Contains(scan, ids[name]) {
			t.Errorf("scan did not list %s (%s)", name, ids[name])
		}
	}

	// Dry run: nothing changes on the server.
	cli("run", "--asset", ids["portrait"])
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
