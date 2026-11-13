package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/mcstatus"
	"github.com/ryancaudill04/minecraft-server/web/internal/ping"
	"github.com/ryancaudill04/minecraft-server/web/internal/world"
	"github.com/ryancaudill04/minecraft-server/web/internal/world/worldtest"
)

const (
	paperAddr  = "paper:25566"
	lazymcAddr = "lazymc:25565"
)

// fakeChecker answers pings from responses; an address that's missing
// fails as if nothing were listening. It counts pings.
func fakeChecker(responses map[string]*ping.Response, pings *atomic.Int32) *mcstatus.Checker {
	return &mcstatus.Checker{
		PaperAddr:  paperAddr,
		LazymcAddr: lazymcAddr,
		Timeout:    time.Second,
		Ping: func(ctx context.Context, addr string) (*ping.Response, error) {
			if pings != nil {
				pings.Add(1)
			}
			if resp, ok := responses[addr]; ok {
				return resp, nil
			}
			return nil, errors.New("connection refused")
		},
	}
}

// newTestServer serves the API with fake pings, the synthetic world, a
// static folder holding index.html, and a shared folder holding base.css.
func newTestServer(t *testing.T, responses map[string]*ping.Response, pings *atomic.Int32) (*httptest.Server, string) {
	t.Helper()
	dir, shared := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>play</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, "base.css"), []byte("body{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := worldtest.Build(t)
	ts := httptest.NewServer(newHandler(dir, shared, server, filepath.Join(server, "plugins", "squaremap", "web"), fakeChecker(responses, pings)))
	t.Cleanup(ts.Close)
	return ts, server
}

func getJSON(t *testing.T, url string, v any) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if v != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			t.Fatal(err)
		}
	}
	return resp
}

func TestAPIStatus(t *testing.T) {
	var pings atomic.Int32
	sleeping := &ping.Response{Players: ping.Players{Max: 20}, Description: ping.Description{Text: "sleeping"}}
	ts, _ := newTestServer(t, map[string]*ping.Response{lazymcAddr: sleeping}, &pings)

	var s mcstatus.Status
	resp := getJSON(t, ts.URL+"/api/status", &s)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status code %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	if s.State != mcstatus.StateSleeping {
		t.Errorf("state = %q, want sleeping", s.State)
	}

	// A second visit within the cache period doesn't ping again.
	before := pings.Load()
	getJSON(t, ts.URL+"/api/status", nil)
	if pings.Load() != before {
		t.Error("a cached status pinged the server again")
	}
}

func TestAPIWorldAndPlayers(t *testing.T) {
	ts, _ := newTestServer(t, nil, nil)

	var info world.Info
	if resp := getJSON(t, ts.URL+"/api/world", &info); resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/world: status code %d", resp.StatusCode)
	}
	if info.Day != 315 || info.Name != "world" {
		t.Errorf("/api/world = %+v", info)
	}

	var players []world.Player
	if resp := getJSON(t, ts.URL+"/api/players", &players); resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/players: status code %d", resp.StatusCode)
	}
	// Carol has neither a name nor a last-seen time, so she's left out.
	if len(players) != 2 || players[0].Name != "Bob" || players[1].Name != "Alice" {
		t.Errorf("/api/players = %+v", players)
	}
}

func TestAPIPlayersNeverSeen(t *testing.T) {
	ts, server := newTestServer(t, nil, nil)
	// Named in usercache.json, Carol is listed, though she has no player
	// data file and so no last-seen time. The players cache loads on the
	// first request, so this is in place in time.
	cache := `[{"uuid":"` + worldtest.CarolID + `","name":"Carol"}]`
	if err := os.WriteFile(filepath.Join(server, "usercache.json"), []byte(cache), 0o644); err != nil {
		t.Fatal(err)
	}

	// The key must be absent: Go's zero time is a truthy date string in JS.
	var raw []map[string]any
	getJSON(t, ts.URL+"/api/players", &raw)
	found := false
	for _, p := range raw {
		if p["id"] != worldtest.CarolID {
			continue
		}
		found = true
		if v, has := p["lastSeen"]; has {
			t.Errorf("Carol has lastSeen %v, want the key left out", v)
		}
	}
	if !found {
		t.Errorf("Carol missing from /api/players: %v", raw)
	}
}

func TestAPIActivity(t *testing.T) {
	ts, server := newTestServer(t, nil, nil)

	var raw []map[string]any
	resp := getJSON(t, ts.URL+"/api/activity", &raw)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status code %d", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	if len(raw) != 9 {
		t.Fatalf("got %d events, want the fixture's 9: %v", len(raw), raw)
	}
	want := map[string]any{
		"kind":        "advancement",
		"at":          "2026-10-20T12:00:00Z",
		"playerId":    worldtest.AliceID,
		"name":        "Alice",
		"advancement": "minecraft:end/elytra",
		"title":       "Sky's the Limit",
	}
	if !reflect.DeepEqual(raw[0], want) {
		t.Errorf("newest event = %v, want %v", raw[0], want)
	}
	// A join has no advancement or title keys.
	last := raw[len(raw)-1]
	if _, has := last["advancement"]; last["kind"] != "joined" || has {
		t.Errorf("oldest event = %v, want Alice's join without an advancement", last)
	}

	// A half-written file only drops that player's events (see #38). The
	// cache still holds the last result, so use a fresh server.
	ts2 := httptest.NewServer(newHandler(t.TempDir(), t.TempDir(), server, t.TempDir(), fakeChecker(nil, nil)))
	t.Cleanup(ts2.Close)
	if err := os.WriteFile(filepath.Join(server, "world", "players", "advancements", worldtest.BobID+".json"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var events []world.Event
	if resp := getJSON(t, ts2.URL+"/api/activity", &events); resp.StatusCode != http.StatusOK {
		t.Fatalf("with an empty file: status code %d", resp.StatusCode)
	}
	if len(events) != 4 {
		t.Errorf("with Bob's file empty: got %d events, want Alice's 4", len(events))
	}
	for _, e := range events {
		if e.PlayerID != worldtest.AliceID {
			t.Errorf("unexpected event %+v", e)
		}
	}
}

func TestAPIActivityLimit(t *testing.T) {
	ts, server := newTestServer(t, nil, nil)
	many := map[string]any{}
	for i := range activityLimit + 10 {
		many[fmt.Sprintf("minecraft:story/step_%02d", i)] = worldtest.Advancement(fmt.Sprintf("2026-11-01 00:%02d:00 +0000", i))
	}
	worldtest.WriteAdvancements(t, server, worldtest.CarolID, many)

	var events []world.Event
	getJSON(t, ts.URL+"/api/activity", &events)
	if len(events) != activityLimit {
		t.Fatalf("got %d events, want %d", len(events), activityLimit)
	}
	if got, want := events[0].Advancement, fmt.Sprintf("minecraft:story/step_%02d", activityLimit+9); got != want {
		t.Errorf("newest = %s, want %s", got, want)
	}
}

func TestAPIUnavailableWorld(t *testing.T) {
	ts, server := newTestServer(t, nil, nil)
	if err := os.Remove(filepath.Join(server, "world", "level.dat")); err != nil {
		t.Fatal(err)
	}

	resp := getJSON(t, ts.URL+"/api/world", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status code %d, want 503", resp.StatusCode)
	}
}

func TestRouting(t *testing.T) {
	ts, _ := newTestServer(t, nil, nil)

	tests := []struct {
		method, path string
		want         int
	}{
		{"GET", "/", http.StatusOK},
		{"GET", "/shared/base.css", http.StatusOK},
		{"GET", "/missing.js", http.StatusNotFound},
		{"POST", "/api/status", http.StatusMethodNotAllowed},
		{"DELETE", "/api/players", http.StatusMethodNotAllowed},
		{"POST", "/api/activity", http.StatusMethodNotAllowed},
		{"GET", "/../../etc/passwd", http.StatusNotFound},
	}
	for _, tt := range tests {
		req, _ := http.NewRequest(tt.method, ts.URL+tt.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tt.want {
			t.Errorf("%s %s = %d, want %d", tt.method, tt.path, resp.StatusCode, tt.want)
		}
	}
}
