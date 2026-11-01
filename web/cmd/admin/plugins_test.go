package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/mcstatus"
	"github.com/ryancaudill04/minecraft-server/web/internal/plugins"
)

// writeJar writes a minimal plugin jar named file into dir and returns
// its SHA-256.
func writeJar(t *testing.T, dir, file, pluginYML string) string {
	t.Helper()
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	w, err := z.Create("plugin.yml")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte(pluginYML))
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(h[:])
}

// newPluginServer serves admin over a server folder whose plugins folder
// is returned.
func newPluginServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	server := t.TempDir()
	dir := filepath.Join(server, "plugins")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	c := &mcstatus.Checker{Timeout: time.Millisecond}
	ts := httptest.NewServer(newHandler(&app{dir: t.TempDir(), sharedDir: t.TempDir(), appsDir: t.TempDir(), serverDir: server, checker: c}))
	t.Cleanup(ts.Close)
	return ts, dir
}

func TestAPIPlugins(t *testing.T) {
	ts, dir := newPluginServer(t)
	sum := writeJar(t, dir, "A-1.jar", "name: A\nversion: 1\nmain: a.A\n")
	writeJar(t, dir, "B-2.jar", "name: B\nversion: 2\nmain: b.B\n")
	manifest := `{"plugins":[{"name":"A","version":"1","file":"A-1.jar","sha256":"` + sum + `","source":"https://example.com/A-1.jar"}]}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	var got Plugins
	if code := getJSON(t, ts.URL+"/api/plugins", &got); code != http.StatusOK {
		t.Fatalf("status code %d", code)
	}
	if !got.Manifest || len(got.Plugins) != 2 {
		t.Fatalf("got %+v, want a manifest and 2 plugins", got)
	}
	if p := got.Plugins[0]; p.File != "A-1.jar" || p.Status != plugins.StatusPinned || p.SHA256 != sum || p.Meta.Name != "A" {
		t.Errorf("A-1.jar: %+v", p)
	}
	if p := got.Plugins[1]; p.File != "B-2.jar" || p.Status != plugins.StatusUnpinned {
		t.Errorf("B-2.jar: %+v", p)
	}
}

func TestAPIPluginsBadManifest(t *testing.T) {
	ts, dir := newPluginServer(t)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	var got Plugins
	if code := getJSON(t, ts.URL+"/api/plugins", &got); code != http.StatusServiceUnavailable {
		t.Errorf("bad manifest: status code %d, want 503", code)
	}
}

// Installing plugins isn't built yet (#76): nothing writes to the plugins
// folder, so every method but GET is refused and nothing is written.
func TestAPIPluginsIsReadOnly(t *testing.T) {
	ts, dir := newPluginServer(t)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for _, p := range []string{"/api/plugins", "/api/plugins/A-1.jar"} {
			req, _ := http.NewRequest(m, ts.URL+p, strings.NewReader("PK\x03\x04"))
			req.Header.Set("Content-Type", "application/java-archive")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 405 or 404", m, p, resp.StatusCode)
			}
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("plugins folder has %d entries after the requests, want 0", len(entries))
	}
}
