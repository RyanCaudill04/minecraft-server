package site

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCache(t *testing.T) {
	var loads atomic.Int32
	c := &Cache[int]{MaxAge: time.Hour, Load: func(context.Context) (int, error) {
		return int(loads.Add(1)), nil
	}}

	for range 3 {
		if v, _ := c.Get(context.Background()); v != 1 {
			t.Fatalf("Get = %d, want the cached 1", v)
		}
	}

	c.MaxAge = 0 // everything is stale now
	if v, _ := c.Get(context.Background()); v != 2 {
		t.Errorf("Get after expiry = %d, want a fresh 2", v)
	}
}

func TestCacheSharesConcurrentLoads(t *testing.T) {
	var loads atomic.Int32
	c := &Cache[int]{MaxAge: time.Hour, Load: func(context.Context) (int, error) {
		loads.Add(1)
		time.Sleep(20 * time.Millisecond)
		return 0, nil
	}}

	done := make(chan struct{})
	for range 20 {
		go func() {
			c.Get(context.Background())
			done <- struct{}{}
		}()
	}
	for range 20 {
		<-done
	}
	if n := loads.Load(); n != 1 {
		t.Errorf("20 concurrent visitors caused %d loads, want 1", n)
	}
}

func TestJSON(t *testing.T) {
	h := JSON("test", &Cache[map[string]int]{MaxAge: time.Hour, Load: func(context.Context) (map[string]int, error) {
		return map[string]int{"n": 1}, nil
	}})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/api/test", nil))

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != `{"n":1}` {
		t.Errorf("body = %s", body)
	}
}

func TestCancelledRequestDoesNotPoisonCache(t *testing.T) {
	// The load fails the way a ping does when its context is cancelled.
	h := JSON("status", &Cache[string]{MaxAge: time.Hour, Load: func(ctx context.Context) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return "active", nil
	}})

	// A visitor who closes the tab cancels their request's context. The load
	// must still run, or a failure would be cached for everyone.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/api/status", nil).WithContext(ctx))

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "active") {
		t.Errorf("got %d %s, want 200 with the loaded value", rec.Code, rec.Body)
	}
}

func TestStatic(t *testing.T) {
	dir, shared := t.TempDir(), t.TempDir()
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "index.html"), "page")
	write(filepath.Join(shared, "base.css"), "shared css")
	for _, sub := range []string{"drafts", "docs"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "drafts", "notes.txt"), "secret")
	write(filepath.Join(dir, "docs", "index.html"), "docs page")
	write(filepath.Join(dir, ".env"), "secret")
	write(filepath.Join(shared, ".hidden.css"), "secret")
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(dir, ".git", "config"), "secret")
	ts := httptest.NewServer(Static(dir, shared))
	t.Cleanup(ts.Close)

	tests := []struct {
		method, path string
		want         int
		body         string
	}{
		{"GET", "/", http.StatusOK, "page"},
		{"GET", "/shared/base.css", http.StatusOK, "shared css"},
		{"GET", "/shared/missing.css", http.StatusNotFound, ""},
		{"GET", "/base.css", http.StatusNotFound, ""},
		// Folders without an index.html aren't listed.
		{"GET", "/shared/", http.StatusNotFound, ""},
		{"GET", "/drafts/", http.StatusNotFound, ""},
		{"GET", "/drafts/notes.txt", http.StatusOK, "secret"},
		{"GET", "/docs/", http.StatusOK, "docs page"},
		// Dotfiles and dot folders aren't published.
		{"GET", "/.env", http.StatusNotFound, ""},
		{"GET", "/.git/config", http.StatusNotFound, ""},
		{"GET", "/shared/.hidden.css", http.StatusNotFound, ""},
		{"POST", "/", http.StatusMethodNotAllowed, ""},
	}
	for _, tt := range tests {
		req, _ := http.NewRequest(tt.method, ts.URL+tt.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tt.want {
			t.Errorf("%s %s = %d, want %d", tt.method, tt.path, resp.StatusCode, tt.want)
		}
		if tt.body != "" && string(body) != tt.body {
			t.Errorf("%s %s body = %q, want %q", tt.method, tt.path, body, tt.body)
		}
		// A release replaces files in place, so nothing may reuse a copy
		// without checking first. (http.FileServer drops the header from
		// its errors, which is fine: only files need it.)
		if got := resp.Header.Get("Cache-Control"); resp.StatusCode == http.StatusOK && got != "no-cache" {
			t.Errorf("%s %s Cache-Control = %q, want %q", tt.method, tt.path, got, "no-cache")
		}
	}
}

// serve has no shared folder. /shared/ must then be an ordinary path in
// dir, not the folder the program started in (here, this package).
func TestStaticWithoutShared(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("page"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := Static(dir, "")

	for path, want := range map[string]int{"/": http.StatusOK, "/shared/site.go": http.StatusNotFound} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
	}
}

func TestServer(t *testing.T) {
	srv := Server("127.0.0.1:0", http.NotFoundHandler())
	if srv.ReadHeaderTimeout == 0 || srv.IdleTimeout == 0 {
		t.Errorf("ReadHeaderTimeout = %v, IdleTimeout = %v; want both set", srv.ReadHeaderTimeout, srv.IdleTimeout)
	}
}
