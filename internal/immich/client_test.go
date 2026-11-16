package immich

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"immich-transcode/internal/immich/api"
)

// Something that is not Immich answering 200 (an auth proxy's login page, a
// wrong server URL) must be reported as an error, not dereferenced.
func TestNonJSONSuccessIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>Sign in</html>"))
	}))
	defer srv.Close()
	c, err := New(srv.URL, "key")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const id = "e5a7d64b-e29c-46d2-bb04-df0044428c85"
	calls := map[string]func() error{
		"ServerVersion":      func() error { _, err := c.ServerVersion(ctx); return err },
		"MissingPermissions": func() error { _, err := c.MissingPermissions(ctx, nil); return err },
		"MyUserID":           func() error { _, err := c.MyUserID(ctx); return err },
		"GetAsset":           func() error { _, err := c.GetAsset(ctx, id); return err },
		"SearchVideos": func() error {
			return c.SearchVideos(ctx, 0, &time.Time{}, func([]Asset) error { return nil })
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil {
				t.Error("no error for an HTML 200 response")
			}
		})
	}
}

// fakeServer answers a few endpoints the way Immich does.
func fakeServer(t *testing.T) *Client {
	t.Helper()
	const id = "e5a7d64b-e29c-46d2-bb04-df0044428c85"
	reply := func(w http.ResponseWriter, status int, body string) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/server/version", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "key" {
			reply(w, http.StatusUnauthorized, `{"message":"Invalid API key"}`)
			return
		}
		reply(w, http.StatusOK, `{"major":3,"minor":3,"patch":1}`)
	})
	mux.HandleFunc("GET /api/api-keys/me", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusOK, `{"permissions":["asset.read"]}`)
	})
	mux.HandleFunc("GET /api/assets/{id}", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusNotFound, `{"message":"Not found"}`)
	})
	mux.HandleFunc("DELETE /api/assets", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	uploads := 0
	mux.HandleFunc("POST /api/assets", func(w http.ResponseWriter, r *http.Request) {
		file, _, err := r.FormFile("assetData")
		if err != nil {
			reply(w, http.StatusBadRequest, `{"message":"no file"}`)
			return
		}
		data, _ := io.ReadAll(file)
		if string(data) != "video bytes" || r.FormValue("filename") != "clip.mp4" {
			reply(w, http.StatusBadRequest, `{"message":"wrong form"}`)
			return
		}
		uploads++
		if uploads > 1 {
			reply(w, http.StatusOK, `{"id":"`+id+`","status":"duplicate"}`)
			return
		}
		reply(w, http.StatusCreated, `{"id":"`+id+`","status":"created"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "key")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClientDecodesResponses(t *testing.T) {
	c := fakeServer(t)
	ctx := context.Background()
	const id = "e5a7d64b-e29c-46d2-bb04-df0044428c85"

	if v, err := c.ServerVersion(ctx); err != nil || v != (Version{3, 3, 1}) {
		t.Errorf("ServerVersion = %v, %v", v, err)
	}
	missing, err := c.MissingPermissions(ctx, []api.Permission{api.AssetRead, api.AssetUpload})
	if err != nil || len(missing) != 1 || missing[0] != api.AssetUpload {
		t.Errorf("MissingPermissions = %v, %v", missing, err)
	}
	// A failed status keeps its code and body: restore relies on telling a
	// 404 from other failures.
	_, err = c.GetAsset(ctx, id)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusNotFound || !strings.Contains(httpErr.Body, "Not found") {
		t.Errorf("GetAsset error = %v, want a 404 HTTPError", err)
	}
	// A success without a body is fine where none is read.
	if err := c.Trash(ctx, id); err != nil {
		t.Errorf("Trash: %v", err)
	}

	path := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(path, []byte("video bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	var sent int64
	req := UploadRequest{Path: path, FileName: "clip.mp4"}
	res, err := c.Upload(ctx, req, func(n int64) { sent += n })
	if err != nil || res.ID != id || res.Duplicate || sent != int64(len("video bytes")) {
		t.Errorf("first Upload = %+v, %v (progress %d)", res, err, sent)
	}
	if res, err := c.Upload(ctx, req, nil); err != nil || !res.Duplicate {
		t.Errorf("second Upload = %+v, %v; want a duplicate", res, err)
	}
}

func TestClientReportsBadKey(t *testing.T) {
	c := fakeServer(t)
	c2, err := New(c.base, "wrong")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c2.ServerVersion(context.Background())
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusUnauthorized {
		t.Errorf("error = %v, want a 401 HTTPError", err)
	}
}
