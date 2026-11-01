package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/control"
	"github.com/ryancaudill04/minecraft-server/web/internal/mcstatus"
	"github.com/ryancaudill04/minecraft-server/web/internal/ping"
	"github.com/ryancaudill04/minecraft-server/web/internal/rcon"
	"github.com/ryancaudill04/minecraft-server/web/internal/rcon/rcontest"
)

// fakeControl records actions and reports a fixed state.
type fakeControl struct {
	mu      sync.Mutex
	actions []control.Action
	state   control.State
	err     error
}

func (f *fakeControl) Do(ctx context.Context, a control.Action) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.actions = append(f.actions, a)
	return nil
}

func (f *fakeControl) State(ctx context.Context) (control.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, nil
}

func (f *fakeControl) done() []control.Action {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]control.Action(nil), f.actions...)
}

// fakeServer is the Minecraft side: what the status pings find, and the
// wakes LazyMC gets.
type fakeServer struct {
	mu    sync.Mutex
	state string // a mcstatus state; offline unless set
	wakes int
	err   error // what wake returns
}

func (f *fakeServer) ping(_ context.Context, addr string) (*ping.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case addr == "paper" && f.state == mcstatus.StateActive:
		return &ping.Response{}, nil
	case addr == "lazymc" && f.state == mcstatus.StateSleeping:
		return &ping.Response{}, nil
	case addr == "lazymc" && f.state == mcstatus.StateStarting:
		r := &ping.Response{}
		r.Description.Text = "Server is starting..."
		return r, nil
	}
	return nil, fmt.Errorf("refused")
}

func (f *fakeServer) wake(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.wakes++
	f.state = mcstatus.StateStarting
	return nil
}

func (f *fakeServer) set(state string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state, f.err = state, err
}

func (f *fakeServer) woken() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.wakes
}

type controlServer struct {
	*httptest.Server
	mc   *fakeServer
	ctl  *fakeControl
	rcon *rcontest.Server
	logs string
	out  *syncBuffer // admin's log
}

// syncBuffer is admin's log output, safe to read while handlers write.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// newControlServer serves admin with a fake controller, a fake RCON server
// and a log file. user is the verified email every request carries, or ""
// for none.
func newControlServer(t *testing.T, user string) *controlServer {
	t.Helper()
	dir, shared, apps := t.TempDir(), t.TempDir(), t.TempDir()
	logs := filepath.Join(t.TempDir(), "latest.log")
	ctl := &fakeControl{state: control.State{Active: "active", Sub: "running"}}
	rs := rcontest.New(t, "pw")
	mc := &fakeServer{}

	out := &syncBuffer{}
	log.SetOutput(out)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	a := &app{
		dir: dir, sharedDir: shared, appsDir: apps,
		checker: &mcstatus.Checker{PaperAddr: "paper", LazymcAddr: "lazymc", Timeout: time.Second, Ping: mc.ping},
		control: ctl,
		wake:    mc.wake,
		rcon: func(ctx context.Context, cmd string) (string, error) {
			return rcon.Run(ctx, rs.Addr, "pw", cmd)
		},
		logPath: logs,
		user:    func(*http.Request) string { return user },
	}
	ts := httptest.NewServer(newHandler(a))
	t.Cleanup(ts.Close)
	return &controlServer{Server: ts, mc: mc, ctl: ctl, rcon: rs, logs: logs, out: out}
}

// post sends a same-origin POST the way the page does, with extra headers
// set or removed (an empty value removes one).
func (s *controlServer) post(t *testing.T, path, body string, headers map[string]string) (int, map[string]string) {
	t.Helper()
	req, _ := http.NewRequest("POST", s.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(CSRFHeader, "1")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got map[string]string
	b, _ := io.ReadAll(resp.Body)
	json.Unmarshal(b, &got)
	return resp.StatusCode, got
}

func TestLockedWithoutUser(t *testing.T) {
	s := newControlServer(t, "")

	var me Me
	if code := getJSON(t, s.URL+"/api/me", &me); code != http.StatusOK {
		t.Fatalf("/api/me: %d", code)
	}
	if me.Control || me.Console || me.Locked == "" {
		t.Errorf("/api/me without a user = %+v, want everything locked", me)
	}

	s.mc.set(mcstatus.StateSleeping, nil)
	for _, path := range []string{"/api/server/start", "/api/server/stop", "/api/server/restart", "/api/server/wake", "/api/console"} {
		code, body := s.post(t, path, `{"command":"stop"}`, nil)
		if code != http.StatusForbidden || body["error"] != lockedNoUser {
			t.Errorf("POST %s without a user = %d %v, want 403", path, code, body)
		}
	}
	resp, err := http.Get(s.URL + "/api/logs")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("GET /api/logs without a user = %d, want 403", resp.StatusCode)
	}
	if got := s.ctl.done(); len(got) != 0 {
		t.Errorf("ran %v without a user", got)
	}
	if got := s.rcon.Commands(); len(got) != 0 {
		t.Errorf("RCON ran %q without a user", got)
	}
	if n := s.mc.woken(); n != 0 {
		t.Errorf("woke the server %d times without a user", n)
	}
}

func TestServerActions(t *testing.T) {
	s := newControlServer(t, "ryan@example.com")

	var me Me
	getJSON(t, s.URL+"/api/me", &me)
	if !me.Control || !me.Console || me.Email != "ryan@example.com" {
		t.Errorf("/api/me = %+v, want everything on for ryan@example.com", me)
	}

	for _, a := range []string{"start", "stop", "restart"} {
		code, body := s.post(t, "/api/server/"+a, "", nil)
		if code != http.StatusAccepted || body["action"] != a {
			t.Errorf("POST /api/server/%s = %d %v, want 202", a, code, body)
		}
	}
	if got := fmt.Sprint(s.ctl.done()); got != "[start stop restart]" {
		t.Errorf("actions = %s", got)
	}
	// Each one is in the audit log with who asked.
	if n := strings.Count(s.out.String(), "audit: ryan@example.com: "); n != 3 {
		t.Errorf("%d audit lines, want 3:\n%s", n, s.out)
	}

	if code, _ := s.post(t, "/api/server/kill", "", nil); code != http.StatusNotFound {
		t.Errorf("POST /api/server/kill = %d, want 404", code)
	}
	if code, _ := s.post(t, "/api/server/admin.service", "", nil); code != http.StatusNotFound {
		t.Errorf("POST /api/server/admin.service = %d, want 404", code)
	}
	resp, _ := http.Get(s.URL + "/api/server/stop")
	resp.Body.Close()
	// GET falls through to the static files, which have no such file.
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /api/server/stop = %d, want 404", resp.StatusCode)
	}
	if got := len(s.ctl.done()); got != 3 {
		t.Errorf("%d actions ran, want still 3", got)
	}
}

func TestServerActionBusy(t *testing.T) {
	s := newControlServer(t, "ryan@example.com")
	s.ctl.err = control.ErrBusy
	if code, _ := s.post(t, "/api/server/start", "", nil); code != http.StatusConflict {
		t.Errorf("POST while busy = %d, want 409", code)
	}
}

func TestWake(t *testing.T) {
	s := newControlServer(t, "ryan@example.com")
	s.mc.set(mcstatus.StateSleeping, nil)

	code, body := s.post(t, "/api/server/wake", "", nil)
	if code != http.StatusAccepted || body["action"] != "wake" {
		t.Fatalf("POST /api/server/wake = %d %v, want 202", code, body)
	}
	if n := s.mc.woken(); n != 1 {
		t.Errorf("woke %d times, want 1", n)
	}
	if !strings.Contains(s.out.String(), "audit: ryan@example.com: wake") {
		t.Errorf("no audit line for the wake:\n%s", s.out)
	}
	// Waking isn't a systemd action.
	if got := s.ctl.done(); len(got) != 0 {
		t.Errorf("systemd actions = %v, want none", got)
	}
	// The status cache was reset, so the page sees Paper starting at once.
	var st Status
	getJSON(t, s.URL+"/api/status", &st)
	if st.State != mcstatus.StateStarting {
		t.Errorf("status after waking = %s, want starting", st.State)
	}
}

// Only a sleeping LazyMC gets a login: a second click while Paper starts,
// or a wake while it's awake or off, is refused and sends nothing.
func TestWakeOnlyWhenSleeping(t *testing.T) {
	s := newControlServer(t, "ryan@example.com")
	for _, state := range []string{mcstatus.StateActive, mcstatus.StateStarting, mcstatus.StateOffline} {
		s.mc.set(state, nil)
		code, body := s.post(t, "/api/server/wake", "", nil)
		if code != http.StatusConflict || body["error"] == "" {
			t.Errorf("wake while %s = %d %v, want 409 with a reason", state, code, body)
		}
	}
	if n := s.mc.woken(); n != 0 {
		t.Errorf("woke %d times, want 0", n)
	}
}

func TestWakeFails(t *testing.T) {
	s := newControlServer(t, "ryan@example.com")
	s.mc.set(mcstatus.StateSleeping, errors.New("connection reset"))
	if code, _ := s.post(t, "/api/server/wake", "", nil); code != http.StatusBadGateway {
		t.Errorf("failed wake = %d, want 502", code)
	}
	if !strings.Contains(s.out.String(), "connection reset") {
		t.Errorf("admin's log doesn't say why:\n%s", s.out)
	}
}

func TestStatusIncludesUnit(t *testing.T) {
	s := newControlServer(t, "")
	s.ctl.state = control.State{Active: "deactivating", Sub: "stop-sigterm"}
	var st Status
	getJSON(t, s.URL+"/api/status", &st)
	if st.State != mcstatus.StateOffline || st.Unit == nil || st.Unit.Active != "deactivating" {
		t.Errorf("/api/status = %+v, unit %+v", st.Status, st.Unit)
	}
}

func TestCSRF(t *testing.T) {
	s := newControlServer(t, "ryan@example.com")
	tests := []struct {
		name    string
		headers map[string]string
	}{
		{"no custom header", map[string]string{CSRFHeader: ""}},
		{"wrong custom header", map[string]string{CSRFHeader: "yes"}},
		{"cross-site fetch", map[string]string{"Sec-Fetch-Site": "cross-site"}},
		{"same-site subdomain", map[string]string{"Sec-Fetch-Site": "same-site"}},
		{"old browser, other origin", map[string]string{"Sec-Fetch-Site": "", "Origin": "https://evil.example"}},
	}
	for _, tt := range tests {
		for _, path := range []string{"/api/server/stop", "/api/console"} {
			if code, _ := s.post(t, path, `{"command":"stop"}`, tt.headers); code != http.StatusForbidden {
				t.Errorf("%s: POST %s = %d, want 403", tt.name, path, code)
			}
		}
	}
	if got := s.ctl.done(); len(got) != 0 {
		t.Errorf("ran %v", got)
	}
	if got := s.rcon.Commands(); len(got) != 0 {
		t.Errorf("RCON ran %q", got)
	}
	// An old browser on the same origin sends Origin matching Host.
	u := strings.TrimPrefix(s.URL, "http://")
	if code, _ := s.post(t, "/api/server/stop", "", map[string]string{"Sec-Fetch-Site": "", "Origin": "http://" + u}); code != http.StatusAccepted {
		t.Errorf("same-origin POST with Origin = %d, want 202", code)
	}
}

func TestConsole(t *testing.T) {
	s := newControlServer(t, "ryan@example.com")
	s.rcon.SetHandle(func(cmd string) string {
		return "§6There are §c1§6 of a max of 20 players online: §fsteve"
	})

	code, body := s.post(t, "/api/console", `{"command":"/list"}`, nil)
	if code != http.StatusOK {
		t.Fatalf("POST /api/console = %d %v", code, body)
	}
	if want := "There are 1 of a max of 20 players online: steve"; body["output"] != want {
		t.Errorf("output = %q, want %q", body["output"], want)
	}
	if got := s.rcon.Commands(); len(got) != 1 || got[0] != "list" {
		t.Errorf("RCON ran %q, want [list]", got)
	}
	if !strings.Contains(s.out.String(), `audit: ryan@example.com: console "list"`) {
		t.Errorf("no audit line:\n%s", s.out)
	}
	if strings.Contains(s.out.String(), "pw") {
		t.Errorf("admin's log mentions the password:\n%s", s.out)
	}

	for name, req := range map[string]string{
		"empty":         `{"command":"  "}`,
		"newline":       `{"command":"say hi\nop steve"}`,
		"not json":      `command=list`,
		"too long":      `{"command":"` + strings.Repeat("a", rcon.MaxCommand+1) + `"}`,
		"huge body":     `{"command":"` + strings.Repeat("a", 10<<10) + `"}`,
		"unknown field": `[1,2]`,
	} {
		if code, _ := s.post(t, "/api/console", req, nil); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, code)
		}
	}
	if code, _ := s.post(t, "/api/console", `{"command":"list"}`, map[string]string{"Content-Type": "text/plain"}); code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain: %d, want 415", code)
	}
	if got := len(s.rcon.Commands()); got != 1 {
		t.Errorf("RCON ran %d commands, want still 1", got)
	}
}

func TestConsoleErrors(t *testing.T) {
	s := newControlServer(t, "ryan@example.com")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().String()
	ln.Close()

	tests := []struct {
		name string
		run  func(ctx context.Context, cmd string) (string, error)
		want int
	}{
		{"asleep", func(ctx context.Context, cmd string) (string, error) { return rcon.Run(ctx, closed, "pw", cmd) }, http.StatusServiceUnavailable},
		{"wrong password", func(ctx context.Context, cmd string) (string, error) { return rcon.Run(ctx, s.rcon.Addr, "nope", cmd) }, http.StatusBadGateway},
		{"timeout", func(ctx context.Context, cmd string) (string, error) {
			return "", fmt.Errorf("rcon: %w", context.DeadlineExceeded)
		}, http.StatusGatewayTimeout},
	}
	for _, tt := range tests {
		h := newHandler(&app{
			dir: t.TempDir(), sharedDir: t.TempDir(), appsDir: t.TempDir(),
			checker: &mcstatus.Checker{},
			rcon:    tt.run,
			user:    func(*http.Request) string { return "ryan@example.com" },
		})
		req := httptest.NewRequest("POST", "/api/console", strings.NewReader(`{"command":"list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(CSRFHeader, "1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tt.want {
			t.Errorf("%s: %d %s, want %d", tt.name, rec.Code, rec.Body, tt.want)
		}
	}

	// No password configured.
	h := newHandler(&app{
		dir: t.TempDir(), sharedDir: t.TempDir(), appsDir: t.TempDir(), checker: &mcstatus.Checker{},
		user: func(*http.Request) string { return "ryan@example.com" },
	})
	req := httptest.NewRequest("POST", "/api/console", strings.NewReader(`{"command":"list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(CSRFHeader, "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no RCON password: %d, want 503", rec.Code)
	}
}

func TestLogs(t *testing.T) {
	s := newControlServer(t, "ryan@example.com")

	var l Logs
	if code := getJSON(t, s.URL+"/api/logs", &l); code != http.StatusOK || l.Lines == nil || len(l.Lines) != 0 {
		t.Errorf("missing log: %d %#v, want 200 and no lines", code, l.Lines)
	}

	var b strings.Builder
	for i := range 300 {
		fmt.Fprintf(&b, "[12:00:%02d INFO]: \x1b[33mline %d\x1b[0m\r\n", i%60, i)
	}
	if err := os.WriteFile(s.logs, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	getJSON(t, s.URL+"/api/logs", &l)
	if len(l.Lines) != defaultLogLines || l.Lines[len(l.Lines)-1] != "[12:00:59 INFO]: line 299" {
		t.Errorf("got %d lines ending %q", len(l.Lines), l.Lines[len(l.Lines)-1])
	}
	getJSON(t, s.URL+"/api/logs?lines=3", &l)
	if len(l.Lines) != 3 || l.Lines[0] != "[12:00:57 INFO]: line 297" {
		t.Errorf("lines=3: %q", l.Lines)
	}
	for _, q := range []string{"0", "-1", "lots"} {
		resp, _ := http.Get(s.URL + "/api/logs?lines=" + q)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("lines=%s: %d, want 400", q, resp.StatusCode)
		}
	}
}

func TestTailLinesLongFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "latest.log")
	// Well over logTailBytes, so only the end is read and the first,
	// partial line is dropped.
	line := strings.Repeat("x", 99) + "\n"
	if err := os.WriteFile(path, []byte(strings.Repeat(line, 2*logTailBytes/len(line))+"last\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err := tailLines(path, maxLogLines)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != maxLogLines || lines[len(lines)-1] != "last" || len(lines[0]) != 99 {
		t.Errorf("got %d lines, first %d bytes, last %q", len(lines), len(lines[0]), lines[len(lines)-1])
	}
}

// Without a user func, the user is whoever the Access check verified, so
// the controls unlock only behind it.
func TestUserFromAccess(t *testing.T) {
	a := &app{
		dir: t.TempDir(), sharedDir: t.TempDir(), appsDir: t.TempDir(), checker: &mcstatus.Checker{},
		control: &fakeControl{state: control.State{Active: "active", Sub: "running"}},
	}
	h := newHandler(a)

	me := func(h http.Handler) Me {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/me", nil))
		var m Me
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("/api/me: %d %s", rec.Code, rec.Body)
		}
		return m
	}
	if m := me(h); m.Email != "" || m.Control {
		t.Errorf("/api/me with no verified user = %+v, want locked", m)
	}
	if m := me(protect(h, authFromEnv("dev@example.com"))); m.Email != "dev@example.com" || !m.Control {
		t.Errorf("/api/me behind the Access check = %+v, want dev@example.com with control", m)
	}
}
