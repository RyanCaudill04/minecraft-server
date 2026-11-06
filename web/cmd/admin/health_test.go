package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/mcstatus"
	"github.com/ryancaudill04/minecraft-server/web/internal/rcon"
	"github.com/ryancaudill04/minecraft-server/web/internal/rcon/rcontest"
)

// Paper's replies, with its § formatting, as RCON returns them.
const (
	paperTPS  = "§6TPS from last 1m, 5m, 15m: §a19.87, §a*20.0, §a20.0"
	paperMSPT = "§6Server tick times §e(§7avg§e/§7min§e/§7max§e)§6 from last 5s§7,§6 10s§7,§6 1m§e:\n" +
		"§6◴ §a4.5§7/§a2.1§7/§a10.3§7, §a4.4§7/§a2.0§7/§a12.1§7, §a4.6§7/§a1.9§7/§a15.0"
)

func TestParseTPS(t *testing.T) {
	tests := []struct {
		out  string
		want TPS
		ok   bool
	}{
		{stripFormatting(paperTPS), TPS{19.87, 20, 20}, true},
		{"TPS from last 1m, 5m, 15m: 12.5, 15.0, 18.25\n", TPS{12.5, 15, 18.25}, true},
		// Some versions add lines after it.
		{"TPS from last 1m, 5m, 15m: 20.0, 20.0, 20.0\nCurrent Memory Usage: 900/2048 mb", TPS{20, 20, 20}, true},
		{"Unknown command. Type \"/help\" for help.", TPS{}, false},
		{"TPS from last 1m, 5m, 15m: 20.0, 20.0", TPS{}, false},
		{"", TPS{}, false},
	}
	for _, tt := range tests {
		got, ok := parseTPS(tt.out)
		if ok != tt.ok || got != tt.want {
			t.Errorf("parseTPS(%q) = %+v, %v; want %+v, %v", tt.out, got, ok, tt.want, tt.ok)
		}
	}
}

func TestParseMSPT(t *testing.T) {
	tests := []struct {
		out  string
		want MSPT
		ok   bool
	}{
		{stripFormatting(paperMSPT), MSPT{Window: "1m", Avg: 4.6, Min: 1.9, Max: 15}, true},
		// No header: the numbers still count, without a window.
		{"◴ 1/2/3", MSPT{Avg: 1, Min: 2, Max: 3}, true},
		{"Unknown command. Type \"/help\" for help.", MSPT{}, false},
		{"", MSPT{}, false},
	}
	for _, tt := range tests {
		got, ok := parseMSPT(tt.out)
		if ok != tt.ok || got != tt.want {
			t.Errorf("parseMSPT(%q) = %+v, %v; want %+v, %v", tt.out, got, ok, tt.want, tt.ok)
		}
	}
}

// writeHost writes a synthetic /proc and /sys and returns its root.
func writeHost(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range map[string]string{
		"sys/class/thermal/thermal_zone0/temp": "61250\n",
		"proc/meminfo":                         "MemTotal: 4000000 kB\nMemFree: 100000 kB\nMemAvailable: 1000000 kB\n",
		"proc/loadavg":                         "1.50 1.00 0.50 2/300 4242\n",
		"proc/uptime":                          "86400.00 100.00\n",
	} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

type healthServer struct {
	*httptest.Server
	mc   *fakeServer
	rcon *rcontest.Server
}

// newHealthServer serves admin with a fake Minecraft server in state, a
// fake RCON server answering like Paper, and hostRoot as /proc and /sys.
func newHealthServer(t *testing.T, state, hostRoot string, withRCON bool) *healthServer {
	t.Helper()
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
	mc := &fakeServer{state: state}
	a := &app{
		dir: t.TempDir(), sharedDir: t.TempDir(), appsDir: t.TempDir(), serverDir: t.TempDir(),
		checker:  &mcstatus.Checker{PaperAddr: "paper", LazymcAddr: "lazymc", Timeout: time.Second, Ping: mc.ping},
		hostRoot: hostRoot,
		user:     func(*http.Request) string { return "ryan@example.com" },
	}
	if withRCON {
		a.rcon = func(ctx context.Context, cmd string) (string, error) {
			return rcon.Run(ctx, rs.Addr, "pw", cmd)
		}
	}
	ts := httptest.NewServer(newHandler(a))
	t.Cleanup(ts.Close)
	return &healthServer{Server: ts, mc: mc, rcon: rs}
}

// getHealth fetches /api/health and returns it decoded, and as raw keys
// so a test can check what's left out.
func getHealth(t *testing.T, url string) (Health, map[string]json.RawMessage) {
	t.Helper()
	resp, err := http.Get(url + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/health = %d %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var h Health
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &h); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	return h, raw
}

func TestHealthAwake(t *testing.T) {
	s := newHealthServer(t, mcstatus.StateActive, writeHost(t), true)
	h, raw := getHealth(t, s.URL)

	if h.TempC == nil || *h.TempC != 61.25 {
		t.Errorf("temp_c = %v, want 61.25", h.TempC)
	}
	if h.Memory == nil || h.Memory.Total != 4000000*1024 || h.Memory.Used != 3000000*1024 {
		t.Errorf("memory = %+v, want 3000000 of 4000000 kB used", h.Memory)
	}
	if h.Load == nil || h.Load.One != 1.5 {
		t.Errorf("load = %+v, want 1.5 over a minute", h.Load)
	}
	if h.UptimeSeconds == nil || *h.UptimeSeconds != 86400 {
		t.Errorf("uptime_seconds = %v, want 86400", h.UptimeSeconds)
	}
	// The disk is the real temporary folder's filesystem.
	if h.Disk == nil || h.Disk.Total == 0 {
		t.Errorf("disk = %+v, want the server folder's filesystem", h.Disk)
	}
	if h.CPUs < 1 || h.Checked.IsZero() {
		t.Errorf("cpus = %d, checked = %v", h.CPUs, h.Checked)
	}
	for _, k := range []string{"temp_c", "memory", "disk", "load", "uptime_seconds", "cpus", "paper", "checked"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("no %q in the reply", k)
		}
	}

	p := h.Paper
	if p.State != mcstatus.StateActive || p.Note != "" {
		t.Errorf("paper = %+v, want active without a note", p)
	}
	if p.TPS == nil || *p.TPS != (TPS{19.87, 20, 20}) {
		t.Errorf("tps = %+v, want 19.87, 20, 20", p.TPS)
	}
	if p.MSPT == nil || *p.MSPT != (MSPT{Window: "1m", Avg: 4.6, Min: 1.9, Max: 15}) {
		t.Errorf("mspt = %+v", p.MSPT)
	}

	// A second request within healthMaxAge is served from the cache.
	getHealth(t, s.URL)
	if got := s.rcon.Commands(); fmt.Sprint(got) != "[tps mspt]" {
		t.Errorf("RCON ran %q, want [tps mspt] once", got)
	}
}

// While Paper sleeps (or starts, or is off) there's no TPS, and RCON isn't
// even tried: nothing that wakes Paper.
func TestHealthNotAwake(t *testing.T) {
	for _, state := range []string{mcstatus.StateSleeping, mcstatus.StateStarting, mcstatus.StateOffline} {
		s := newHealthServer(t, state, writeHost(t), true)
		h, raw := getHealth(t, s.URL)
		if h.Paper.State != state || h.Paper.TPS != nil || h.Paper.MSPT != nil || h.Paper.Note != "" {
			t.Errorf("%s: paper = %+v, want just the state", state, h.Paper)
		}
		var paper map[string]json.RawMessage
		json.Unmarshal(raw["paper"], &paper)
		if len(paper) != 1 {
			t.Errorf("%s: paper = %s, want only state", state, raw["paper"])
		}
		if h.Memory == nil {
			t.Errorf("%s: the Pi's readings should still be there", state)
		}
		if got := s.rcon.Commands(); len(got) != 0 {
			t.Errorf("%s: RCON ran %q, want nothing", state, got)
		}
		if got := s.mc.woken(); got != 0 {
			t.Errorf("%s: woke Paper %d times", state, got)
		}
	}
}

func TestHealthRCONProblems(t *testing.T) {
	// No password: active, with a note instead of TPS.
	s := newHealthServer(t, mcstatus.StateActive, writeHost(t), false)
	if h, _ := getHealth(t, s.URL); h.Paper.TPS != nil || h.Paper.Note != noteNoRCON {
		t.Errorf("no RCON: paper = %+v, want note %q", h.Paper, noteNoRCON)
	}

	// The ping says active but RCON is gone, as when Paper just stopped.
	s = newHealthServer(t, mcstatus.StateActive, writeHost(t), true)
	s.rcon.Close()
	if h, _ := getHealth(t, s.URL); h.Paper.TPS != nil || h.Paper.Note != noteUnreachable {
		t.Errorf("RCON closed: paper = %+v, want note %q", h.Paper, noteUnreachable)
	}

	// A server without Paper's commands: no TPS or MSPT, and a note.
	s = newHealthServer(t, mcstatus.StateActive, writeHost(t), true)
	s.rcon.SetHandle(func(string) string { return "Unknown command" })
	if h, _ := getHealth(t, s.URL); h.Paper.TPS != nil || h.Paper.MSPT != nil || h.Paper.Note != noteFailed {
		t.Errorf("unknown commands: paper = %+v, want note %q", h.Paper, noteFailed)
	}
}

// With no /proc or /sys (macOS, or a container), the Pi's readings are
// left out, not errors.
func TestHealthMissingSources(t *testing.T) {
	s := newHealthServer(t, mcstatus.StateSleeping, t.TempDir(), true)
	h, raw := getHealth(t, s.URL)
	for _, k := range []string{"temp_c", "memory", "load", "uptime_seconds"} {
		if _, ok := raw[k]; ok {
			t.Errorf("%q = %s, want it left out", k, raw[k])
		}
	}
	if h.Disk == nil || h.Paper.State != mcstatus.StateSleeping {
		t.Errorf("disk %+v, paper %+v: want both still there", h.Disk, h.Paper)
	}
}

func TestHealthRouting(t *testing.T) {
	s := newHealthServer(t, mcstatus.StateOffline, t.TempDir(), false)
	resp, err := http.Post(s.URL+"/api/health", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/health = %d, want 405", resp.StatusCode)
	}
}
