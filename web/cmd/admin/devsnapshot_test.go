package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/backups"
	"github.com/ryancaudill04/minecraft-server/web/internal/control"
	"github.com/ryancaudill04/minecraft-server/web/internal/mcstatus"
	"github.com/ryancaudill04/minecraft-server/web/internal/ping"
	"github.com/ryancaudill04/minecraft-server/web/internal/rcon"
	"github.com/ryancaudill04/minecraft-server/web/internal/rcon/rcontest"
)

// devUser is who the dev site shows as signed in.
const devUser = "dev@example.com"

// TestDevSnapshot saves what admin's API answers for a made-up server, as
// one JSON file per route. The dev site on Cloudflare (dev/) serves these
// files in place of the API, so everything here is synthetic: the players,
// the plugins, the release, the backups, the log and the Pi's readings.
// scripts/build-dev.sh runs it with DEV_SNAPSHOT_DIR set to the site's
// folder. Without it the files go to a temporary folder, so the test still
// checks that every route the page reads answers.
func TestDevSnapshot(t *testing.T) {
	out := os.Getenv("DEV_SNAPSHOT_DIR")
	if out == "" {
		out = t.TempDir()
	}

	server := t.TempDir()
	writeServerFile(t, server, "whitelist.json", testWhitelist)
	writeServerFile(t, server, "ops.json", testOps)
	writeServerFile(t, server, "server.properties", "white-list=true\n")
	writeJoined(t, server,
		Listed{UUID: "00000000-0000-4000-8000-00000000000a", Name: "Alice"},
		Listed{UUID: "00000000-0000-4000-8000-00000000000b", Name: "bob_42"},
		Listed{UUID: "00000000-0000-4000-8000-00000000000d", Name: "dave"})

	pluginDir := filepath.Join(server, "plugins")
	if err := os.Mkdir(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := writeJar(t, pluginDir, "Chunky-1.4.jar", "name: Chunky\nversion: 1.4\nmain: a.Chunky\n")
	writeJar(t, pluginDir, "Unpinned-0.1.jar", "name: Unpinned\nversion: 0.1\nmain: b.Unpinned\n")
	writeServerFile(t, pluginDir, "manifest.json", `{"plugins":[{"name":"Chunky","version":"1.4","file":"Chunky-1.4.jar","sha256":"`+sum+`","source":"https://example.com/Chunky-1.4.jar"}]}`)

	logs := filepath.Join(t.TempDir(), "latest.log")
	writeServerFile(t, filepath.Dir(logs), "latest.log", "[10:00:00 INFO]: Done (4.2s)! For help, type \"help\"\n"+
		"[10:05:12 INFO]: Alice joined the game\n"+
		"[10:07:40 INFO]: bob_42 joined the game\n")

	apps := t.TempDir()
	now := time.Now().UTC()
	activate(t, apps, "r42",
		`{"tag":"r42","kind":"feature","released_at":"`+now.Add(-2*time.Hour).Format(time.RFC3339)+`","changes":[
			{"pr":1,"title":"A made-up feature for the dev site","kind":"feature"},
			{"pr":2,"title":"A made-up fix","kind":"fix"}]}`,
		`{"tag":"r42","applied_at":"`+now.Add(-110*time.Minute).Format(time.RFC3339)+`"}`)
	if err := os.MkdirAll(filepath.Join(apps, "releases", "r41"), 0o755); err != nil {
		t.Fatal(err)
	}

	backupDir := t.TempDir()
	for _, day := range []int{3, 1} {
		name := "world-" + now.AddDate(0, 0, -day).Format("20060102") + "-043000.tar.gz"
		writeServerFile(t, backupDir, name, "synthetic backup")
		writeServerFile(t, backupDir, name+".sha256", "synthetic checksum")
		if day == 3 {
			writeServerFile(t, backupDir, name+".keep", "")
		}
	}

	rs := rcontest.New(t, "pw")
	rs.SetHandle(func(cmd string) string {
		switch cmd {
		case "tps":
			return paperTPS
		case "mspt":
			return paperMSPT
		}
		return "Unknown command"
	})
	a := &app{
		dir: t.TempDir(), sharedDir: t.TempDir(), appsDir: apps, serverDir: server,
		checker: &mcstatus.Checker{PaperAddr: "paper", LazymcAddr: "lazymc", Timeout: time.Second, Ping: activePing},
		control: &fakeControl{state: control.State{Active: "active", Sub: "running"}},
		wake:    func(context.Context) error { return nil },
		rcon: func(ctx context.Context, cmd string) (string, error) {
			return rcon.Run(ctx, rs.Addr, "pw", cmd)
		},
		logPath:    logs,
		backups:    &backups.Store{Dir: backupDir, KeepDays: 10},
		backupUnit: &fakeControl{state: control.State{Active: "inactive", Sub: "dead"}},
		hostRoot:   writeHost(t),
		user:       func(*http.Request) string { return devUser },
	}
	writeSnapshot(t, newHandler(a), filepath.Join(out, "api"),
		"status", "health", "deploy", "plugins", "me", "logs", "backups", "players")
}

// activePing answers like an awake Paper with Alice and bob_42 online.
func activePing(_ context.Context, addr string) (*ping.Response, error) {
	if addr != "paper" {
		return nil, errors.New("refused")
	}
	return &ping.Response{
		Version: ping.Version{Name: "Paper 26.2"},
		Players: ping.Players{Online: 2, Max: 20, Sample: []ping.Player{
			{Name: "Alice", ID: "00000000-0000-4000-8000-00000000000a"},
			{Name: "bob_42", ID: "00000000-0000-4000-8000-00000000000b"},
		}},
	}, nil
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
