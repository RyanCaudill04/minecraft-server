package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/listing"
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
	reg, err := listing.Open("", listing.DefaultGrace)
	if err != nil {
		t.Fatal(err)
	}
	return newTestServerWith(t, responses, pings, reg)
}

// newTestServerWith is newTestServer with a given listing registry.
func newTestServerWith(t *testing.T, responses map[string]*ping.Response, pings *atomic.Int32, reg *listing.Registry) (*httptest.Server, string) {
	t.Helper()
	dir, shared := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>play</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, "base.css"), []byte("body{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := worldtest.Build(t)
	ts := httptest.NewServer(newHandler(dir, shared, server, fakeChecker(responses, pings), reg))
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
	// Nobody has been listed yet, so nobody is shown; the count still is.
	if players == nil || len(players) != 0 {
		t.Errorf("/api/players = %+v, want []", players)
	}
	if info.Players != 3 {
		t.Errorf("/api/world players = %d, want 3", info.Players)
	}
}

// activePaper is Paper answering with the given sample and online count.
func activePaper(online int, sample ...ping.Player) map[string]*ping.Response {
	return map[string]*ping.Response{
		paperAddr:  {Version: ping.Version{Name: "Paper 26.2"}, Players: ping.Players{Online: online, Max: 20, Sample: sample}},
		lazymcAddr: {Players: ping.Players{Online: online, Max: 20}},
	}
}

var (
	listedAlice = ping.Player{Name: "Alice", ID: worldtest.AliceID}
	anonymous   = ping.Player{Name: "Anonymous Player", ID: mcstatus.AnonymousID}
)

func get(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status code %d", url, resp.StatusCode)
	}
	return string(body)
}

// Alice is listed; Bob is online but hidden (anonymous in the sample);
// Carol was never listed. Only Alice may appear, in every endpoint.
func TestHiddenPlayersNeverAppear(t *testing.T) {
	ts, _ := newTestServer(t, activePaper(2, listedAlice, anonymous), nil)

	var s mcstatus.Status
	getJSON(t, ts.URL+"/api/status", &s)
	if s.Online != 2 || len(s.Players) != 1 || s.Players[0].ID != worldtest.AliceID {
		t.Errorf("/api/status = %+v, want 2 online with only Alice named", s)
	}

	var players []world.Player
	getJSON(t, ts.URL+"/api/players", &players)
	if len(players) != 1 || players[0].ID != worldtest.AliceID || players[0].Name != "Alice" {
		t.Errorf("/api/players = %+v, want only Alice", players)
	}

	hidden := []string{"Bob", worldtest.BobID, "Carol", worldtest.CarolID, worldtest.CarolID[:8], mcstatus.AnonymousID, "Anonymous"}
	for _, path := range []string{"/api/status", "/api/world", "/api/players", "/"} {
		body := get(t, ts.URL+path)
		for _, h := range hidden {
			if strings.Contains(body, h) {
				t.Errorf("GET %s contains %q:\n%s", path, h, body)
			}
		}
	}
}

// A player listed once who later plays hidden drops out: their newer data
// on disk is newer than the last listing.
func TestPlayerWhoHidesLaterDropsOut(t *testing.T) {
	reg, err := listing.Open("", listing.DefaultGrace)
	if err != nil {
		t.Fatal(err)
	}
	// Bob was listed a day before his last session, which he played hidden.
	bob := mcstatus.Player{Name: "Bob", ID: worldtest.BobID}
	if err := reg.Observe([]mcstatus.Player{bob}, worldtest.BobSeen.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	ts, _ := newTestServerWith(t, nil, nil, reg)
	if body := get(t, ts.URL+"/api/players"); strings.Contains(body, worldtest.BobID) || strings.Contains(body, "Bob") {
		t.Errorf("/api/players shows Bob after he played hidden: %s", body)
	}

	// Listed during that session (just before his logout save): shown.
	if err := reg.Observe([]mcstatus.Player{bob}, worldtest.BobSeen.Add(-10*time.Second)); err != nil {
		t.Fatal(err)
	}
	var players []world.Player
	getJSON(t, ts.URL+"/api/players", &players)
	if len(players) != 1 || players[0].ID != worldtest.BobID {
		t.Errorf("/api/players = %+v, want only Bob", players)
	}
}

// Only Paper's sample counts: LazyMC answering (asleep) teaches nothing,
// even if it sent names.
func TestOnlyPaperListsPlayers(t *testing.T) {
	sleeping := map[string]*ping.Response{lazymcAddr: {Players: ping.Players{Max: 20, Sample: []ping.Player{listedAlice}}}}
	ts, _ := newTestServer(t, sleeping, nil)
	get(t, ts.URL+"/api/status")
	if body := get(t, ts.URL+"/api/players"); body != "[]\n" {
		t.Errorf("/api/players = %s, want []", body)
	}
}

// watch learns who is listed with nobody visiting the page.
func TestWatch(t *testing.T) {
	reg, err := listing.Open("", listing.DefaultGrace)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // one check, then return
	watch(ctx, fakeChecker(activePaper(1, listedAlice), nil), reg, time.Hour)
	if !reg.Public(worldtest.AliceID, worldtest.AliceSeen) {
		t.Error("watch didn't record Alice as listed")
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
