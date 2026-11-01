package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/ping"
	"github.com/ryancaudill04/minecraft-server/web/internal/world"
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
	if data, err := os.ReadFile(filepath.Join(state, "visible.json")); err != nil || string(data) != `["Alice"]` {
		t.Errorf("visible.json = %q, %v", data, err)
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
