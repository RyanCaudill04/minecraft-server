package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ryancaudill04/minecraft-server/web/internal/mcstatus"
	"github.com/ryancaudill04/minecraft-server/web/internal/rcon"
)

// Synthetic players: made-up names and UUIDs.
const (
	testWhitelist = `[
  {"uuid": "00000000-0000-4000-8000-00000000000b", "name": "bob_42"},
  {"uuid": "00000000-0000-4000-8000-00000000000a", "name": "Alice"},
  {"uuid": "00000000-0000-4000-8000-00000000000c", "name": "carol"}
]`
	testOps = `[
  {"uuid": "00000000-0000-4000-8000-00000000000a", "name": "Alice", "level": 4, "bypassesPlayerLimit": false}
]`
	testProperties = `#Minecraft server properties
#Mon Jan 01 00:00:00 UTC 2024
enforce-whitelist=false
rcon.password=hunter2
white-list = true
motd=A server
`
)

func writeServerFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadPlayers(t *testing.T) {
	dir := t.TempDir()

	// A server that has never started has none of the files.
	p, err := readPlayers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Whitelist == nil || p.Ops == nil || len(p.Whitelist)+len(p.Ops) != 0 || p.WhiteList != nil || p.Enforce != nil {
		t.Errorf("empty folder = %+v, want two empty lists and no settings", p)
	}

	writeServerFile(t, dir, "whitelist.json", testWhitelist)
	writeServerFile(t, dir, "ops.json", testOps)
	writeServerFile(t, dir, "server.properties", testProperties)
	p, err = readPlayers(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, l := range p.Whitelist {
		names = append(names, l.Name)
	}
	// Sorted by name, ignoring case.
	if got := strings.Join(names, ","); got != "Alice,bob_42,carol" {
		t.Errorf("whitelist = %s", got)
	}
	if p.Whitelist[0].UUID != "00000000-0000-4000-8000-00000000000a" {
		t.Errorf("Alice's UUID = %q", p.Whitelist[0].UUID)
	}
	if len(p.Ops) != 1 || p.Ops[0] != (Op{Name: "Alice", UUID: "00000000-0000-4000-8000-00000000000a", Level: 4}) {
		t.Errorf("ops = %+v", p.Ops)
	}
	if p.WhiteList == nil || !*p.WhiteList || p.Enforce == nil || *p.Enforce {
		t.Errorf("white-list %v, enforce-whitelist %v, want true and false", p.WhiteList, p.Enforce)
	}
}

func TestReadPlayersOddFiles(t *testing.T) {
	dir := t.TempDir()
	writeServerFile(t, dir, "whitelist.json", "null")
	writeServerFile(t, dir, "ops.json", "\n")
	writeServerFile(t, dir, "server.properties", "white-list=maybe\nenforce-whitelist\n")
	p, err := readPlayers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Whitelist == nil || p.Ops == nil || len(p.Whitelist)+len(p.Ops) != 0 {
		t.Errorf("null and empty files = %+v, want two empty lists", p)
	}
	if p.WhiteList != nil || p.Enforce != nil {
		t.Errorf("settings that aren't true or false = %v %v, want unknown", p.WhiteList, p.Enforce)
	}

	writeServerFile(t, dir, "ops.json", `{"not": "a list"}`)
	if _, err := readPlayers(dir); err == nil || !strings.Contains(err.Error(), "ops.json") {
		t.Errorf("ops.json that isn't a list: err = %v, want one naming the file", err)
	}
}

// Only the keys asked for come back: server.properties also holds the
// RCON password.
func TestReadPropertiesKeepsOnlyKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.properties")
	writeServerFile(t, filepath.Dir(path), "server.properties", testProperties+"rcon.password=true\n")
	got, err := readProperties(path, "white-list")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["white-list"] == nil {
		t.Errorf("got %v, want only white-list", got)
	}
}

func TestValidName(t *testing.T) {
	for _, name := range []string{"Bob", "bob_42", "___", "ABCDEFGHIJKLMNOP", "a1_"} {
		if !validName.MatchString(name) {
			t.Errorf("%q refused, want accepted", name)
		}
	}
	for _, name := range []string{
		"", "ab", "ABCDEFGHIJKLMNOPQ", "bob 42", "bob\nop eve", "bob\n", "bob;op", "bob-42",
		"bób", "@a", "@p", "../bob", "bob\x00", " bob", "bob ",
	} {
		if validName.MatchString(name) {
			t.Errorf("%q accepted, want refused", name)
		}
	}
}

func TestAPIPlayers(t *testing.T) {
	s := newControlServer(t, "ryan@example.com")
	writeServerFile(t, s.serverDir, "whitelist.json", testWhitelist)
	writeServerFile(t, s.serverDir, "ops.json", testOps)
	writeServerFile(t, s.serverDir, "server.properties", testProperties)

	var p Players
	if code := getJSON(t, s.URL+"/api/players", &p); code != http.StatusOK {
		t.Fatalf("GET /api/players = %d", code)
	}
	if len(p.Whitelist) != 3 || len(p.Ops) != 1 || p.WhiteList == nil || !*p.WhiteList {
		t.Errorf("GET /api/players = %+v", p)
	}

	// The reply never carries anything else from server.properties.
	resp, err := http.Get(s.URL + "/api/players")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(b), "hunter2") || strings.Contains(string(b), "rcon") {
		t.Errorf("reply mentions RCON: %s", b)
	}

	writeServerFile(t, s.serverDir, "whitelist.json", "{")
	if code := getJSON(t, s.URL+"/api/players", &p); code != http.StatusServiceUnavailable {
		t.Errorf("broken whitelist.json = %d, want 503", code)
	}
}

func TestChangePlayer(t *testing.T) {
	s := newControlServer(t, "ryan@example.com")
	s.rcon.SetHandle(func(cmd string) string { return "§eDone: " + cmd })

	for path, want := range map[string]string{
		"/api/players/whitelist/add":    "whitelist add bob_42",
		"/api/players/whitelist/remove": "whitelist remove bob_42",
		"/api/players/ops/add":          "op bob_42",
		"/api/players/ops/remove":       "deop bob_42",
	} {
		before := len(s.rcon.Commands())
		code, body := s.post(t, path, `{"name":"bob_42"}`, nil)
		if code != http.StatusOK || body["output"] != "Done: "+want {
			t.Errorf("POST %s = %d %v, want 200 and Paper's reply", path, code, body)
		}
		cmds := s.rcon.Commands()
		if len(cmds) != before+1 || cmds[len(cmds)-1] != want {
			t.Errorf("POST %s ran %q, want %q", path, cmds[before:], want)
		}
		if !strings.Contains(s.out.String(), "audit: ryan@example.com: "+want) {
			t.Errorf("no audit line for %q:\n%s", want, s.out)
		}
	}
}

// Nothing but a valid name reaches RCON.
func TestChangePlayerRefusesBadRequests(t *testing.T) {
	s := newControlServer(t, "ryan@example.com")

	for _, name := range []string{
		`""`, `"ab"`, `"ABCDEFGHIJKLMNOPQ"`, `"bob op eve"`, `"bob\nop eve"`, `"bob\u0000"`,
		`"@a"`, `"bob;stop"`, `"bób"`, `null`, `42`,
	} {
		code, body := s.post(t, "/api/players/ops/add", `{"name":`+name+`}`, nil)
		if code != http.StatusBadRequest || body["error"] == "" {
			t.Errorf("name %s = %d %v, want 400 with a reason", name, code, body)
		}
	}
	for name, req := range map[string]string{
		"not json":  `name=bob`,
		"a list":    `["bob"]`,
		"huge body": `{"name":"` + strings.Repeat("a", 10<<10) + `"}`,
	} {
		if code, _ := s.post(t, "/api/players/whitelist/add", req, nil); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, code)
		}
	}
	if code, _ := s.post(t, "/api/players/whitelist/add", `{"name":"bob"}`, map[string]string{"Content-Type": "text/plain"}); code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain: %d, want 415", code)
	}
	for _, path := range []string{
		"/api/players/whitelist/on", "/api/players/whitelist/off", "/api/players/whitelist/reload",
		"/api/players/bans/add", "/api/players/ops/op", "/api/players/whitelist",
	} {
		if code, _ := s.post(t, path, `{"name":"bob"}`, nil); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d, want 404 or 405", path, code)
		}
	}
	if got := s.rcon.Commands(); len(got) != 0 {
		t.Errorf("RCON ran %q, want nothing", got)
	}
}

func TestPlayersNeedAUser(t *testing.T) {
	s := newControlServer(t, "")
	writeServerFile(t, s.serverDir, "whitelist.json", testWhitelist)

	resp, err := http.Get(s.URL + "/api/players")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("GET /api/players without a user = %d, want 403", resp.StatusCode)
	}
	for _, path := range []string{"/api/players/whitelist/add", "/api/players/ops/remove"} {
		code, body := s.post(t, path, `{"name":"bob"}`, nil)
		if code != http.StatusForbidden || body["error"] != lockedNoUser {
			t.Errorf("POST %s without a user = %d %v, want 403", path, code, body)
		}
	}
	if got := s.rcon.Commands(); len(got) != 0 {
		t.Errorf("RCON ran %q without a user", got)
	}
}

func TestPlayersMethodsAndCSRF(t *testing.T) {
	s := newControlServer(t, "ryan@example.com")

	// A GET of a change falls through to the static files, which have no
	// such file; a POST to the list isn't allowed.
	resp, err := http.Get(s.URL + "/api/players/ops/add?name=bob")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /api/players/ops/add = %d, want 404", resp.StatusCode)
	}
	if code, _ := s.post(t, "/api/players", `{"name":"bob"}`, nil); code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/players = %d, want 405", code)
	}

	for name, headers := range map[string]map[string]string{
		"no custom header":  {CSRFHeader: ""},
		"cross-site fetch":  {"Sec-Fetch-Site": "cross-site"},
		"same-site":         {"Sec-Fetch-Site": "same-site"},
		"other origin":      {"Sec-Fetch-Site": "", "Origin": "https://evil.example"},
		"wrong header text": {CSRFHeader: "true"},
	} {
		if code, _ := s.post(t, "/api/players/ops/add", `{"name":"bob"}`, headers); code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", name, code)
		}
	}
	if got := s.rcon.Commands(); len(got) != 0 {
		t.Errorf("RCON ran %q", got)
	}
}

// While Paper sleeps RCON doesn't answer: the change is refused with a
// reason, and the files aren't touched.
func TestChangePlayerAsleep(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().String()
	ln.Close()

	server := t.TempDir()
	writeServerFile(t, server, "whitelist.json", testWhitelist)
	tests := []struct {
		name string
		run  func(ctx context.Context, cmd string) (string, error)
		want int
	}{
		{"asleep", func(ctx context.Context, cmd string) (string, error) { return rcon.Run(ctx, closed, "pw", cmd) }, http.StatusServiceUnavailable},
		{"no RCON password", nil, http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		h := newHandler(&app{
			dir: t.TempDir(), sharedDir: t.TempDir(), appsDir: t.TempDir(), serverDir: server,
			checker: &mcstatus.Checker{},
			rcon:    tt.run,
			user:    func(*http.Request) string { return "ryan@example.com" },
		})
		req := httptest.NewRequest("POST", "/api/players/whitelist/remove", strings.NewReader(`{"name":"bob_42"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(CSRFHeader, "1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tt.want || !strings.Contains(rec.Body.String(), "error") {
			t.Errorf("%s: %d %s, want %d with a reason", tt.name, rec.Code, rec.Body, tt.want)
		}
		if tt.name == "asleep" && !strings.Contains(rec.Body.String(), "asleep") {
			t.Errorf("asleep: reply %s doesn't say Paper is asleep", rec.Body)
		}

		// The list still reads from the files.
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/players", nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "bob_42") {
			t.Errorf("%s: GET /api/players = %d %s, want bob_42 still listed", tt.name, rec.Code, rec.Body)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(server, "whitelist.json")); string(b) != testWhitelist {
		t.Errorf("whitelist.json changed while asleep:\n%s", b)
	}
}
