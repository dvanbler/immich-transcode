package immich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
