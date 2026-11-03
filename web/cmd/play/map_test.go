package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile writes body to dir/name, making folders as needed.
func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newMapServer lays out a server folder with squaremap's web folder in it,
// plus a secret next to it that must never be served, and returns play's
// handler for it.
func newMapServer(t *testing.T) (http.Handler, string) {
	t.Helper()
	server := t.TempDir()
	web := filepath.Join(server, "plugins", "squaremap", "web")
	writeFile(t, server, "server.properties", "rcon.password=secret")
	writeFile(t, web, "index.html", "<div id=map></div>")
	writeFile(t, web, "assets/index.js", "console.log(1)")
	writeFile(t, web, "tiles/settings.json", `{"ui":{}}`)
	writeFile(t, web, "tiles/players.json", `{"players":[{"name":"Steve","x":10,"z":20}],"max":20}`)
	writeFile(t, web, "tiles/minecraft_overworld/3/0_0.png", "\x89PNG\r\n\x1a\nfake")
	return newHandler(t.TempDir(), t.TempDir(), server, web, fakeChecker(nil, nil)), web
}

// get sends a GET for target, which may hold anything a client could put
// on the request line.
func get(h http.Handler, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestMapServesPageAndTiles(t *testing.T) {
	h, _ := newMapServer(t)

	tests := []struct {
		path, body, cache string
	}{
		{"/map/", "<div id=map></div>", "no-cache"},
		{"/map/assets/index.js", "console.log(1)", "no-cache"},
		{"/map/tiles/settings.json", `{"ui":{}}`, "no-cache"},
		{"/map/tiles/minecraft_overworld/3/0_0.png", "\x89PNG\r\n\x1a\nfake", "public, max-age=300"},
	}
	for _, tt := range tests {
		rec := get(h, tt.path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200", tt.path, rec.Code)
			continue
		}
		if rec.Body.String() != tt.body {
			t.Errorf("%s: body %q, want %q", tt.path, rec.Body, tt.body)
		}
		if got := rec.Header().Get("Cache-Control"); got != tt.cache {
			t.Errorf("%s: Cache-Control %q, want %q", tt.path, got, tt.cache)
		}
		if got := rec.Header().Get("Content-Security-Policy"); got != mapCSP {
			t.Errorf("%s: Content-Security-Policy %q", tt.path, got)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options %q", tt.path, got)
		}
	}
	if ct := get(h, "/map/tiles/minecraft_overworld/3/0_0.png").Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("tile Content-Type %q, want image/png", ct)
	}
}

func TestMapRedirectsToSlash(t *testing.T) {
	h, _ := newMapServer(t)
	rec := get(h, "/map")
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/map/" {
		t.Errorf("/map: %d to %q, want 301 to /map/", rec.Code, rec.Header().Get("Location"))
	}
}

func TestMapMissingTile(t *testing.T) {
	h, _ := newMapServer(t)
	rec := get(h, "/map/tiles/minecraft_overworld/3/9_9.png")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing tile: status %d, want 404", rec.Code)
	}
	// A missing tile may be rendered soon, so its 404 isn't cached.
	if got := rec.Header().Get("Cache-Control"); strings.Contains(got, "max-age") {
		t.Errorf("missing tile: Cache-Control %q, want no max-age", got)
	}
}

func TestMapNotRenderedYet(t *testing.T) {
	server := t.TempDir()
	h := newHandler(t.TempDir(), t.TempDir(), server, filepath.Join(server, "nope"), fakeChecker(nil, nil))
	for _, p := range []string{"/map/", "/map/tiles/minecraft_overworld/0/0_0.png"} {
		rec := get(h, p)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", p, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "hasn't been rendered") {
			t.Errorf("%s: body %q", p, rec.Body)
		}
	}
}

func TestMapStaysInsideItsFolder(t *testing.T) {
	h, web := newMapServer(t)

	// A symlink inside the folder must not lead out of it.
	if err := os.Symlink(filepath.Join(web, "..", "..", "..", "server.properties"), filepath.Join(web, "tiles", "link.png")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(web, "..", ".."), filepath.Join(web, "up")); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{
		"/map/../server.properties",
		"/map/..%2f..%2f..%2fserver.properties",
		"/map/tiles/..%2f..%2f..%2f..%2fserver.properties",
		"/map/%2e%2e/%2e%2e/%2e%2e/server.properties",
		"/map/tiles/link.png",
		"/map/up/server.properties",
		"/map/up/squaremap/web/index.html",
		"/map/..\\..\\..\\server.properties",
	} {
		rec := get(h, p)
		body, _ := io.ReadAll(rec.Body)
		if strings.Contains(string(body), "secret") {
			t.Errorf("%s: served the secret (status %d)", p, rec.Code)
		}
		if rec.Code == http.StatusOK {
			t.Errorf("%s: status 200, want an error", p)
		}
	}
}

func TestMapNoListings(t *testing.T) {
	h, web := newMapServer(t)
	for _, p := range []string{"/map/tiles/", "/map/tiles/minecraft_overworld/3/", "/map/assets/"} {
		rec := get(h, p)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", p, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "0_0.png") || strings.Contains(rec.Body.String(), "index.js") {
			t.Errorf("%s: listed the folder: %q", p, rec.Body)
		}
	}

	// Without index.html the top folder isn't listed either.
	if err := os.Remove(filepath.Join(web, "index.html")); err != nil {
		t.Fatal(err)
	}
	if rec := get(h, "/map/"); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "tiles") {
		t.Errorf("/map/ without index.html: %d %q, want 404", rec.Code, rec.Body)
	}
}

func TestMapHidesPlayers(t *testing.T) {
	h, _ := newMapServer(t)
	for _, p := range []string{"/map/tiles/players.json", "/map/tiles/./players.json", "/map/tiles/Players.json"} {
		rec := get(h, p)
		if strings.Contains(rec.Body.String(), "Steve") {
			t.Errorf("%s: showed a player's position: %q", p, rec.Body)
		}
	}
	rec := get(h, "/map/tiles/players.json")
	if rec.Code != http.StatusOK || rec.Body.String() != noPlayers {
		t.Errorf("players.json: %d %q, want 200 %q", rec.Code, rec.Body, noPlayers)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("players.json: Cache-Control %q, want no-store", got)
	}
}

func TestMapIsReadOnly(t *testing.T) {
	h, _ := newMapServer(t)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(m, "/map/tiles/settings.json", strings.NewReader("x")))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status %d, want 405", m, rec.Code)
		}
	}
}

// The main page frames the map for its preview, so the map allows play's own
// pages as ancestors and nobody else's.
func TestMapFramedOnlyBySelf(t *testing.T) {
	h, _ := newMapServer(t)
	csp := get(h, "/map/").Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'self'") {
		t.Errorf("Content-Security-Policy %q, want frame-ancestors 'self'", csp)
	}
}
