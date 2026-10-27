package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/ping"
	"github.com/ryancaudill04/minecraft-server/web/internal/world"
	"github.com/ryancaudill04/minecraft-server/web/internal/world/worldtest"
)

const (
	paperAddr  = "paper:25566"
	lazymcAddr = "lazymc:25565"
)

var errDown = errors.New("connection refused")

// fakeChecker answers pings from responses; an address that's missing
// fails as if nothing were listening. It counts pings.
func fakeChecker(responses map[string]*ping.Response, pings *atomic.Int32) *checker {
	return &checker{
		paperAddr:  paperAddr,
		lazymcAddr: lazymcAddr,
		timeout:    time.Second,
		status: func(ctx context.Context, addr string) (*ping.Response, error) {
			if pings != nil {
				pings.Add(1)
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if resp, ok := responses[addr]; ok {
				return resp, nil
			}
			return nil, errDown
		},
	}
}

func response(motd string, online int, names ...string) *ping.Response {
	r := &ping.Response{
		Version:     ping.Version{Name: "Paper 26.2"},
		Players:     ping.Players{Online: online, Max: 20},
		Description: ping.Description{Text: motd},
	}
	for _, n := range names {
		r.Players.Sample = append(r.Players.Sample, ping.Player{Name: n, ID: n + "-id"})
	}
	return r
}

func TestCheckStates(t *testing.T) {
	tests := []struct {
		name      string
		responses map[string]*ping.Response
		want      Status
	}{
		{
			name: "paper answers: active",
			responses: map[string]*ping.Response{
				paperAddr:  response("A Minecraft Server", 2, "Alice", "Bob"),
				lazymcAddr: response("A Minecraft Server", 2),
			},
			want: Status{State: stateActive, Online: 2, Max: 20, Version: "Paper 26.2",
				Players: []Player{{"Alice", "Alice-id"}, {"Bob", "Bob-id"}}},
		},
		{
			name:      "only lazymc answers: sleeping",
			responses: map[string]*ping.Response{lazymcAddr: response("§7Server is sleeping\n§aJoin to wake it up", 0)},
			want:      Status{State: stateSleeping, Max: 20, Version: "Paper 26.2", Players: []Player{}},
		},
		{
			name:      "lazymc says starting",
			responses: map[string]*ping.Response{lazymcAddr: response("§eServer is Starting...", 0)},
			want:      Status{State: stateStarting, Max: 20, Version: "Paper 26.2", Players: []Player{}},
		},
		{
			name: "nothing answers: offline",
			want: Status{State: stateOffline, Players: []Player{}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fakeChecker(tt.responses, nil).check(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got.Checked.IsZero() {
				t.Error("Checked is not set")
			}
			got.Checked = time.Time{}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(tt.want)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("check:\n got %s\nwant %s", gotJSON, wantJSON)
			}
		})
	}
}

func TestCache(t *testing.T) {
	var loads atomic.Int32
	c := &cache[int]{maxAge: time.Hour, load: func(context.Context) (int, error) {
		return int(loads.Add(1)), nil
	}}

	for range 3 {
		if v, _ := c.get(context.Background()); v != 1 {
			t.Fatalf("get = %d, want the cached 1", v)
		}
	}

	c.maxAge = 0 // everything is stale now
	if v, _ := c.get(context.Background()); v != 2 {
		t.Errorf("get after expiry = %d, want a fresh 2", v)
	}
}

func TestCacheSharesConcurrentLoads(t *testing.T) {
	var loads atomic.Int32
	c := &cache[int]{maxAge: time.Hour, load: func(context.Context) (int, error) {
		loads.Add(1)
		time.Sleep(20 * time.Millisecond)
		return 0, nil
	}}

	done := make(chan struct{})
	for range 20 {
		go func() {
			c.get(context.Background())
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

// newTestServer serves the API with fake pings, the synthetic world, and a
// static folder holding index.html.
func newTestServer(t *testing.T, responses map[string]*ping.Response, pings *atomic.Int32) (*httptest.Server, string) {
	t.Helper()
	site := t.TempDir()
	if err := os.WriteFile(filepath.Join(site, "index.html"), []byte("<h1>play</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := worldtest.Build(t)
	ts := httptest.NewServer(newHandler(site, server, fakeChecker(responses, pings)))
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
	ts, _ := newTestServer(t, map[string]*ping.Response{lazymcAddr: response("sleeping", 0)}, &pings)

	var s Status
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
	if s.State != stateSleeping {
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
	if len(players) != 3 || players[0].Name != "Bob" {
		t.Errorf("/api/players = %+v", players)
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

func TestCancelledRequestDoesNotPoisonCache(t *testing.T) {
	c := fakeChecker(map[string]*ping.Response{paperAddr: response("", 1, "Alice")}, nil)
	h := serveJSON("status", &cache[Status]{maxAge: time.Hour, load: c.check})

	// A visitor who closes the tab cancels their request's context. The ping
	// must still run, or "offline" would be cached for everyone.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/api/status", nil).WithContext(ctx))

	if !strings.Contains(rec.Body.String(), `"state":"active"`) {
		t.Errorf("body = %s, want the active state", rec.Body)
	}
}
