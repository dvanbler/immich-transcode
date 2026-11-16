// Package immich is a thin, typed layer over the generated Immich client.
package immich

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/dvanbler/immich-transcode/internal/immich/api"
)

// SpecVersion is the Immich version whose OpenAPI spec generated api.
const SpecVersion = "3.3.1"

// MinMajor is the lowest server major version supported.
const MinMajor = 3

type Asset = api.AssetResponseDto

// Client talks to one Immich server with one API key.
type Client struct {
	api  *api.Client
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
	c, err := api.NewClient(base, api.WithHTTPClient(hc), auth)
	if err != nil {
		return nil, err
	}
	return &Client{api: c, base: base}, nil
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

// maxErrorBody bounds how much of a failed response an HTTPError keeps.
const maxErrorBody = 500

// read consumes and closes a response, turning a transport error or a
// non-2xx status into an error. It takes the (response, error) pair of a
// generated client call directly.
func read(op string, resp *http.Response, err error) ([]byte, error) {
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s: read response: %w", op, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &HTTPError{Op: op, Status: resp.StatusCode, Body: string(body[:min(len(body), maxErrorBody)])}
	}
	return body, nil
}

// decode reads a successful response whose body is a JSON T. Anything else
// answering 2xx (an auth proxy's login page, a server URL that is not
// Immich) is an error.
func decode[T any](op string, resp *http.Response, err error) (*T, error) {
	body, err := read(op, resp, err)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "json") {
		return nil, fmt.Errorf("%s: HTTP %d response is not the JSON Immich sends; check the server URL and any proxy in front of Immich", op, resp.StatusCode)
	}
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("%s: decode response: %w", op, err)
	}
	return &v, nil
}

// discard reads a response only for its status.
func discard(op string, resp *http.Response, err error) error {
	_, err = read(op, resp, err)
	return err
}

// Version is a server version.
type Version struct{ Major, Minor, Patch int }

func (v Version) String() string { return fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch) }

// ServerVersion returns the server's version.
func (c *Client) ServerVersion(ctx context.Context) (Version, error) {
	resp, err := c.api.GetServerVersion(ctx)
	v, err := decode[api.ServerVersionResponseDto]("get server version", resp, err)
	if err != nil {
		return Version{}, err
	}
	return Version{v.Major, v.Minor, v.Patch}, nil
}

// MissingPermissions returns which of want the API key lacks.
func (c *Client) MissingPermissions(ctx context.Context, want []api.Permission) ([]api.Permission, error) {
	resp, err := c.api.GetMyApiKey(ctx)
	key, err := decode[api.ApiKeyResponseDto]("get API key", resp, err)
	if err != nil {
		return nil, err
	}
	have := map[api.Permission]bool{}
	for _, p := range key.Permissions {
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
	resp, err := c.api.GetMyUser(ctx)
	user, err := decode[api.UserAdminResponseDto]("get current user", resp, err)
	if err != nil {
		return "", err
	}
	return user.Id.String(), nil
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
		resp, err := c.api.SearchAssets(ctx, nil, req)
		res, err := decode[api.SearchResponseDto]("search assets", resp, err)
		if err != nil {
			return err
		}
		if err := fn(res.Assets.Items); err != nil {
			return err
		}
		next := res.Assets.NextCursor
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
	resp, err := c.api.GetAssetInfo(ctx, uid, nil)
	return decode[Asset]("get asset "+id, resp, err)
}

// Download streams the original file of asset id into w and returns the
// base64 SHA-1 of the bytes written.
func (c *Client) Download(ctx context.Context, id string, w io.Writer, progress func(int64)) (string, int64, error) {
	uid, err := parseID(id)
	if err != nil {
		return "", 0, err
	}
	f := false
	resp, err := c.api.DownloadAsset(ctx, uid, &api.DownloadAssetParams{Edited: &f})
	if err != nil {
		return "", 0, fmt.Errorf("download %s: %w", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
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
	written := make(chan struct{})
	go func() {
		defer close(written)
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
	resp, err := c.api.UploadAssetWithBody(ctx, params, mw.FormDataContentType(), pr)
	// Closing the read end fails any write still pending, so the goroutine
	// is guaranteed to finish; wait for it before f is closed.
	pr.CloseWithError(errors.New("upload finished"))
	<-written
	res, err := decode[api.AssetMediaResponseDto]("upload", resp, err)
	if err != nil {
		return UploadResult{}, err
	}
	return UploadResult{ID: res.Id.String(), Duplicate: res.Status == api.Duplicate}, nil
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
	resp, err := c.api.CopyAsset(ctx, api.AssetCopyDto{
		SourceId: src, TargetId: dst,
		Albums: &t, Favorite: &t, SharedLinks: &t, Stack: &t, Sidecar: &t,
	})
	return discard("copy asset", resp, err)
}

// UpdateAsset applies a partial update. PUT /assets/{id} is marked deprecated
// in the v3.2.4 and v3.3.1 specs, with no replacement yet.
func (c *Client) UpdateAsset(ctx context.Context, id string, dto api.UpdateAssetDto) error {
	uid, err := parseID(id)
	if err != nil {
		return err
	}
	resp, err := c.api.UpdateAsset(ctx, uid, dto)
	return discard("update asset "+id, resp, err)
}

// TagAsset adds tags to an asset.
func (c *Client) TagAsset(ctx context.Context, id string, tagIDs []openapi_types.UUID) error {
	uid, err := parseID(id)
	if err != nil {
		return err
	}
	resp, err := c.api.BulkTagAssets(ctx, api.TagBulkAssetsDto{AssetIds: []openapi_types.UUID{uid}, TagIds: tagIDs})
	return discard("tag asset "+id, resp, err)
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
	resp, err := c.api.DeleteAssets(ctx, api.AssetBulkDeleteDto{Ids: uids, Force: &f})
	return discard("trash assets", resp, err)
}

// Restore moves assets out of the trash.
func (c *Client) Restore(ctx context.Context, id string) error {
	u, err := parseID(id)
	if err != nil {
		return err
	}
	resp, err := c.api.RestoreAssets(ctx, api.BulkIdsDto{Ids: []openapi_types.UUID{u}})
	return discard("restore asset "+id, resp, err)
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
