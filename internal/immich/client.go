// Package immich is a thin, typed layer over the generated Immich client.
package immich

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"immich-transcode/internal/immich/api"
)

// SpecVersion is the Immich version whose OpenAPI spec generated api.
const SpecVersion = "3.2.4"

// MinMajor is the lowest server major version supported.
const MinMajor = 3

type Asset = api.AssetResponseDto

// Client talks to one Immich server with one API key.
type Client struct {
	api  *api.ClientWithResponses
	raw  *api.Client
	base string
}

// New creates a client for serverURL (with or without the /api suffix).
func New(serverURL, apiKey string) (*Client, error) {
	if apiKey == "" {
		return nil, errors.New("no API key: set the IMMICH_API_KEY environment variable")
	}
	base := strings.TrimRight(serverURL, "/")
	if !strings.HasSuffix(base, "/api") {
		base += "/api"
	}
	// No overall timeout: originals can be many GB. Requests are bounded by
	// their contexts instead.
	hc := &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ResponseHeaderTimeout: 10 * time.Minute,
		IdleConnTimeout:       90 * time.Second,
	}}
	auth := api.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("Accept", "application/json")
		return nil
	})
	c, err := api.NewClientWithResponses(base, api.WithHTTPClient(hc), auth)
	if err != nil {
		return nil, err
	}
	raw, err := api.NewClient(base, api.WithHTTPClient(hc), auth)
	if err != nil {
		return nil, err
	}
	return &Client{api: c, raw: raw, base: base}, nil
}

// HTTPError is a non-2xx response.
type HTTPError struct {
	Op     string
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s: HTTP %d: %s", e.Op, e.Status, strings.TrimSpace(e.Body))
}

// generatedResponse is implemented by every *XxxResponse type that
// oapi-codegen produces; each also has a Body []byte field.
type generatedResponse interface{ StatusCode() int }

// check turns a transport error or non-2xx response into an error.
func check(op string, r generatedResponse, err error) error {
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if code := r.StatusCode(); code < 200 || code > 299 {
		b := reflect.ValueOf(r).Elem().FieldByName("Body").Bytes()
		if len(b) > 500 {
			b = b[:500]
		}
		return &HTTPError{Op: op, Status: code, Body: string(b)}
	}
	return nil
}

// Version is a server version.
type Version struct{ Major, Minor, Patch int }

func (v Version) String() string { return fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch) }

// ServerVersion returns the server's version.
func (c *Client) ServerVersion(ctx context.Context) (Version, error) {
	r, err := c.api.GetServerVersionWithResponse(ctx)
	if err := check("get server version", r, err); err != nil {
		return Version{}, err
	}
	return Version{r.JSON200.Major, r.JSON200.Minor, r.JSON200.Patch}, nil
}

// MissingPermissions returns which of want the API key lacks.
func (c *Client) MissingPermissions(ctx context.Context, want []api.Permission) ([]api.Permission, error) {
	r, err := c.api.GetMyApiKeyWithResponse(ctx)
	if err := check("get API key", r, err); err != nil {
		return nil, err
	}
	have := map[api.Permission]bool{}
	for _, p := range r.JSON200.Permissions {
		have[p] = true
	}
	if have[api.All] {
		return nil, nil
	}
	var missing []api.Permission
	for _, p := range want {
		if !have[p] {
			missing = append(missing, p)
		}
	}
	return missing, nil
}

// MyUserID returns the id of the API key's owner.
func (c *Client) MyUserID(ctx context.Context) (string, error) {
	r, err := c.api.GetMyUserWithResponse(ctx)
	if err := check("get current user", r, err); err != nil {
		return "", err
	}
	return r.JSON200.Id.String(), nil
}

// SearchVideos pages through videos of at least minSize bytes, largest
// first, calling fn for each page. A non-nil takenBefore limits it to videos
// taken before that instant. Trashed assets are excluded by the server
// (search never returns them unless asked) and again by the caller.
func (c *Client) SearchVideos(ctx context.Context, minSize int64, takenBefore *time.Time, fn func([]Asset) error) error {
	video := api.VIDEO
	minSz := float64(minSize)
	vis := []api.AssetVisibility{api.Timeline, api.Archive}
	field := api.SearchOrderField("fileSizeInBytes")
	dir := api.AssetOrder("desc")
	size := 250
	t := true
	req := api.MetadataSearchDto{
		Filter: &api.SearchFilter{
			Type:            &api.EnumFilterAssetType{Eq: &video},
			FileSizeInBytes: &api.NumberFilter{Gte: &minSz},
			Visibility:      &api.EnumFilterAssetVisibility{In: &vis},
			IsOffline:       &api.BoolFilter{Eq: false},
		},
		OrderBy:  &api.SearchOrder{Field: &field, Direction: &dir},
		Size:     &size,
		WithExif: &t,
	}
	if takenBefore != nil {
		req.Filter.TakenAt = &api.DateFilter{Lt: takenBefore}
	}
	for {
		r, err := c.api.SearchAssetsWithResponse(ctx, nil, req)
		if err := check("search assets", r, err); err != nil {
			return err
		}
		if err := fn(r.JSON200.Assets.Items); err != nil {
			return err
		}
		next := r.JSON200.Assets.NextCursor
		if next == nil || *next == "" {
			return nil
		}
		req.Cursor = next
	}
}

// GetAsset fetches one asset with EXIF, people and tags.
func (c *Client) GetAsset(ctx context.Context, id string) (*Asset, error) {
	uid, err := parseID(id)
	if err != nil {
		return nil, err
	}
	r, err := c.api.GetAssetInfoWithResponse(ctx, uid, nil)
	if err := check("get asset "+id, r, err); err != nil {
		return nil, err
	}
	return r.JSON200, nil
}

// Download streams the original file of asset id into w and returns the
// base64 SHA-1 of the bytes written.
func (c *Client) Download(ctx context.Context, id string, w io.Writer, progress func(int64)) (string, int64, error) {
	uid, err := parseID(id)
	if err != nil {
		return "", 0, err
	}
	f := false
	resp, err := c.raw.DownloadAsset(ctx, uid, &api.DownloadAssetParams{Edited: &f})
	if err != nil {
		return "", 0, fmt.Errorf("download %s: %w", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return "", 0, &HTTPError{Op: "download " + id, Status: resp.StatusCode, Body: string(b)}
	}
	h := sha1.New()
	n, err := io.Copy(io.MultiWriter(w, h, progressWriter(progress)), resp.Body)
	if err != nil {
		return "", n, fmt.Errorf("download %s: %w", id, err)
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil)), n, nil
}

// UploadRequest describes a new asset upload.
type UploadRequest struct {
	Path           string
	FileName       string
	FileCreatedAt  time.Time
	FileModifiedAt time.Time
	SHA1Hex        string
	// Visibility and IsFavorite are set at upload so the deprecated asset
	// update endpoint is not needed for them. Empty visibility is omitted.
	Visibility api.AssetVisibility
	IsFavorite bool
}

// UploadResult is the server's response.
type UploadResult struct {
	ID        string
	Duplicate bool
}

// Upload streams a file to POST /assets as multipart form data.
func (c *Client) Upload(ctx context.Context, u UploadRequest, progress func(int64)) (UploadResult, error) {
	f, err := os.Open(u.Path)
	if err != nil {
		return UploadResult{}, err
	}
	defer f.Close()

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := writeUploadForm(mw, f, u, progress)
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()

	params := &api.UploadAssetParams{}
	if u.SHA1Hex != "" {
		params.XImmichChecksum = &u.SHA1Hex
	}
	resp, err := c.raw.UploadAssetWithBody(ctx, params, mw.FormDataContentType(), pr)
	pr.CloseWithError(errors.New("upload finished"))
	if err != nil {
		return UploadResult{}, fmt.Errorf("upload: %w", err)
	}
	r, err := api.ParseUploadAssetResponse(resp)
	if err := check("upload", r, err); err != nil {
		return UploadResult{}, err
	}
	switch {
	case r.JSON201 != nil:
		return UploadResult{ID: r.JSON201.Id.String()}, nil
	case r.JSON200 != nil:
		return UploadResult{ID: r.JSON200.Id.String(), Duplicate: r.JSON200.Status == api.Duplicate}, nil
	}
	return UploadResult{}, fmt.Errorf("upload: unexpected response %d: %s", resp.StatusCode, r.Body)
}

func writeUploadForm(mw *multipart.Writer, f *os.File, u UploadRequest, progress func(int64)) error {
	fields := []struct{ k, v string }{
		{"fileCreatedAt", u.FileCreatedAt.UTC().Format(time.RFC3339Nano)},
		{"fileModifiedAt", u.FileModifiedAt.UTC().Format(time.RFC3339Nano)},
		{"filename", u.FileName},
		{"isFavorite", strconv.FormatBool(u.IsFavorite)},
	}
	if u.Visibility != "" {
		fields = append(fields, struct{ k, v string }{"visibility", string(u.Visibility)})
	}
	for _, kv := range fields {
		if err := mw.WriteField(kv.k, kv.v); err != nil {
			return err
		}
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="assetData"; filename=%q`, u.FileName))
	h.Set("Content-Type", "application/octet-stream")
	part, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	_, err = io.Copy(io.MultiWriter(part, progressWriter(progress)), f)
	return err
}

// CopyRelationships copies albums, favorite, shared links, stack and sidecar
// from one asset to another (PUT /assets/copy).
func (c *Client) CopyRelationships(ctx context.Context, from, to string) error {
	src, err := parseID(from)
	if err != nil {
		return err
	}
	dst, err := parseID(to)
	if err != nil {
		return err
	}
	t := true
	r, err := c.api.CopyAssetWithResponse(ctx, api.AssetCopyDto{
		SourceId: src, TargetId: dst,
		Albums: &t, Favorite: &t, SharedLinks: &t, Stack: &t, Sidecar: &t,
	})
	return check("copy asset", r, err)
}

// UpdateAsset applies a partial update. PUT /assets/{id} is marked deprecated
// in the v3.2.4 spec but no replacement exists there yet.
func (c *Client) UpdateAsset(ctx context.Context, id string, dto api.UpdateAssetDto) error {
	uid, err := parseID(id)
	if err != nil {
		return err
	}
	r, err := c.api.UpdateAssetWithResponse(ctx, uid, dto)
	return check("update asset "+id, r, err)
}

// TagAsset adds tags to an asset.
func (c *Client) TagAsset(ctx context.Context, id string, tagIDs []openapi_types.UUID) error {
	uid, err := parseID(id)
	if err != nil {
		return err
	}
	r, err := c.api.BulkTagAssetsWithResponse(ctx, api.TagBulkAssetsDto{AssetIds: []openapi_types.UUID{uid}, TagIds: tagIDs})
	return check("tag asset "+id, r, err)
}

// Trash moves assets to the trash. It never force-deletes.
func (c *Client) Trash(ctx context.Context, ids ...string) error {
	var uids []openapi_types.UUID
	for _, id := range ids {
		u, err := parseID(id)
		if err != nil {
			return err
		}
		uids = append(uids, u)
	}
	f := false
	r, err := c.api.DeleteAssetsWithResponse(ctx, api.AssetBulkDeleteDto{Ids: uids, Force: &f})
	return check("trash assets", r, err)
}

// Restore moves assets out of the trash.
func (c *Client) Restore(ctx context.Context, id string) error {
	u, err := parseID(id)
	if err != nil {
		return err
	}
	r, err := c.api.RestoreAssetsWithResponse(ctx, api.BulkIdsDto{Ids: []openapi_types.UUID{u}})
	return check("restore asset "+id, r, err)
}

// FileSHA1 returns the hex and base64 SHA-1 of a file.
func FileSHA1(path string) (hexSum, b64Sum string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", "", err
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum), base64.StdEncoding.EncodeToString(sum), nil
}

func parseID(id string) (openapi_types.UUID, error) {
	var u openapi_types.UUID
	if err := u.UnmarshalText([]byte(id)); err != nil {
		return u, fmt.Errorf("invalid asset id %q", id)
	}
	return u, nil
}

type progressWriter func(int64)

func (p progressWriter) Write(b []byte) (int, error) {
	if p != nil {
		p(int64(len(b)))
	}
	return len(b), nil
}
