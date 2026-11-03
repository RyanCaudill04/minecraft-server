package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/ping"
	"github.com/ryancaudill04/minecraft-server/web/internal/world"
	"github.com/ryancaudill04/minecraft-server/web/internal/world/worldtest"
)

// writeLatestLog writes a made-up session to the server's logs/latest.log,
// stamped with the current time of day.
func writeLatestLog(t *testing.T, server string, msgs ...string) {
	t.Helper()
	now := time.Now()
	clock := now.Format("15:04:05")
	content := fmt.Sprintf("[%s] [Server thread/INFO]: Done (3.000s)! For help, type \"help\"\n", clock)
	for _, m := range msgs {
		content += fmt.Sprintf("[%s] [Server thread/INFO]: %s\n", clock, m)
	}
	path := filepath.Join(server, "logs", "latest.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}
}

func active(names ...string) map[string]*ping.Response {
	var sample []ping.Player
	for _, n := range names {
		id := anonymousID
		if n != "Anonymous Player" {
			id = "id-" + n
		}
		sample = append(sample, ping.Player{Name: n, ID: id})
	}
	return map[string]*ping.Response{paperAddr: {Players: ping.Players{Online: len(names), Max: 20, Sample: sample}}}
}

func TestAPIActivity(t *testing.T) {
	state := t.TempDir()
	t.Setenv("STATE_DIRECTORY", state)
	ts, server := newTestServer(t, active("Alice", "Anonymous Player"), nil)
	writeLatestLog(t, server,
		"Alice joined the game",
		"Bob joined the game",
		"Alice has made the advancement [Stone Age]",
		"Bob was slain by Zombie",
		"Alice was slain by Bob",
	)

	var got Activity
	resp := getJSON(t, ts.URL+"/api/activity", &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status code %d", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	// Bob is hidden from the list, so neither his events nor his name show.
	if len(got.Events) != 2 || got.Events[0].Title != "Stone Age" || got.Events[1].Kind != "join" || got.Events[1].Player != "Alice" {
		t.Errorf("/api/activity = %+v", got)
	}
	if data, err := os.ReadFile(filepath.Join(state, "visible.json")); err != nil || !regexp.MustCompile(`^\["Alice@\d+"\]$`).Match(data) {
		t.Errorf("visible.json = %q, %v", data, err)
	}
}

// Nobody requests the feed while the player is on; watch still confirms
// the visit from the server list.
func TestWatchConfirmsVisits(t *testing.T) {
	server := worldtest.Build(t)
	dir, shared := t.TempDir(), t.TempDir()
	h, poll := newApp(dir, shared, server, filepath.Join(server, "plugins", "squaremap", "web"), fakeChecker(active("Alice", "Anonymous Player"), nil))
	writeLatestLog(t, server, "Alice joined the game", "Bob joined the game", "Bob was slain by Zombie")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { watch(ctx, time.Millisecond, poll); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	ts := httptest.NewServer(h)
	defer ts.Close()
	var got Activity
	getJSON(t, ts.URL+"/api/activity", &got)
	if len(got.Events) != 1 || got.Events[0].Player != "Alice" {
		t.Errorf("/api/activity = %+v; want only Alice's join", got)
	}
}

func TestAPIActivityWithoutLogs(t *testing.T) {
	ts, _ := newTestServer(t, nil, nil)
	var got map[string]any
	if resp := getJSON(t, ts.URL+"/api/activity", &got); resp.StatusCode != http.StatusOK {
		t.Fatalf("status code %d", resp.StatusCode)
	}
	if events, ok := got["events"].([]any); !ok || len(events) != 0 {
		t.Errorf("/api/activity = %v; want an empty events list", got)
	}
}

func TestAPITime(t *testing.T) {
	t.Run("sleeping", func(t *testing.T) {
		ts, _ := newTestServer(t, nil, nil)
		var now world.Now
		if resp := getJSON(t, ts.URL+"/api/time", &now); resp.StatusCode != http.StatusOK {
			t.Fatalf("status code %d", resp.StatusCode)
		}
		if now.Live || now.TicksPerSecond != 0 || now.Day != 315 || now.TimeOfDay != 5453 || !now.Raining {
			t.Errorf("/api/time = %+v; want the saved sky", now)
		}
	})
	t.Run("active", func(t *testing.T) {
		ts, server := newTestServer(t, active("Alice"), nil)
		writeLatestLog(t, server)
		var now world.Now
		if resp := getJSON(t, ts.URL+"/api/time", &now); resp.StatusCode != http.StatusOK {
			t.Fatalf("status code %d", resp.StatusCode)
		}
		if !now.Live || now.TicksPerSecond != 20 || now.Day != 315 || now.TimeOfDay < 5453 {
			t.Errorf("/api/time = %+v; want a live clock", now)
		}
	})
	t.Run("answers are for the moment they're sent", func(t *testing.T) {
		ts, server := newTestServer(t, active("Alice"), nil)
		writeLatestLog(t, server)
		var a, b world.Now
		getJSON(t, ts.URL+"/api/time", &a)
		time.Sleep(1100 * time.Millisecond) // served from the cache
		getJSON(t, ts.URL+"/api/time", &b)
		if !b.At.After(a.At) {
			t.Errorf("At did not advance: %v then %v", a.At, b.At)
		}
		if ta, tb := a.Day*24000+a.TimeOfDay, b.Day*24000+b.TimeOfDay; tb < ta+20 {
			t.Errorf("clock went %d -> %d ticks; want at least 20 more after 1s", ta, tb)
		}
	})
	t.Run("missing world", func(t *testing.T) {
		ts, server := newTestServer(t, nil, nil)
		if err := os.RemoveAll(filepath.Join(server, "world")); err != nil {
			t.Fatal(err)
		}
		var now world.Now
		if resp := getJSON(t, ts.URL+"/api/time", &now); resp.StatusCode != http.StatusOK || now.Live {
			t.Errorf("/api/time = %d %+v; want the empty saved sky", resp.StatusCode, now)
		}
	})
}
