package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ryancaudill04/minecraft-server/web/internal/world/worldtest"
)

// TestDevSnapshot saves what play's API answers for the synthetic world,
// with the server active, as one JSON file per route. The dev site on
// Cloudflare (dev/) serves these files in place of the API.
// scripts/build-dev.sh runs it with DEV_SNAPSHOT_DIR set to the site's
// folder. Without it the files go to a temporary folder, so the test still
// checks that every route the page reads answers.
func TestDevSnapshot(t *testing.T) {
	out := os.Getenv("DEV_SNAPSHOT_DIR")
	if out == "" {
		out = t.TempDir()
	}
	server := worldtest.Build(t)
	active := fixtureStates[0]
	if active.name != "active" {
		t.Fatalf("fixtureStates[0] is %q, want active", active.name)
	}
	h := newHandler(t.TempDir(), t.TempDir(), server, filepath.Join(server, "plugins", "squaremap", "web"), fakeChecker(active.responses, nil))
	writeSnapshot(t, h, filepath.Join(out, "api"), "status", "world", "players", "activity")
}

// writeSnapshot GETs /api/<name> from h for each name and writes the body
// to dir/<name>.json. Every route must answer 200 with JSON.
func writeSnapshot(t *testing.T, h http.Handler, dir string, names ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/"+name, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET /api/%s: status %d: %s", name, rec.Code, rec.Body)
			continue
		}
		if !json.Valid(rec.Body.Bytes()) {
			t.Errorf("GET /api/%s: not JSON: %s", name, rec.Body)
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name+".json"), rec.Body.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
