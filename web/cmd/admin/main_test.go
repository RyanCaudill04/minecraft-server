package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/access"
	"github.com/ryancaudill04/minecraft-server/web/internal/mcstatus"
	"github.com/ryancaudill04/minecraft-server/web/internal/ping"
)

// newTestServer serves admin with a Paper that answers, a static folder
// holding index.html, a shared folder holding base.css, and an empty apps
// folder, which it returns.
func newTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir, shared, apps := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>admin</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, "base.css"), []byte("body{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &mcstatus.Checker{
		PaperAddr:  "paper:25566",
		LazymcAddr: "lazymc:25565",
		Timeout:    time.Second,
		Ping: func(ctx context.Context, addr string) (*ping.Response, error) {
			if addr == "paper:25566" {
				return &ping.Response{Players: ping.Players{Online: 1, Max: 20}}, nil
			}
			return nil, errors.New("connection refused")
		},
	}
	ts := httptest.NewServer(newHandler(dir, shared, apps, c))
	t.Cleanup(ts.Close)
	return ts, apps
}

func getJSON(t *testing.T, url string, v any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}

func TestAPIStatus(t *testing.T) {
	ts, _ := newTestServer(t)

	var s mcstatus.Status
	if code := getJSON(t, ts.URL+"/api/status", &s); code != http.StatusOK {
		t.Fatalf("status code %d", code)
	}
	if s.State != mcstatus.StateActive || s.Online != 1 {
		t.Errorf("/api/status = %+v, want active with 1 online", s)
	}
}

func TestAPIDeploy(t *testing.T) {
	ts, apps := newTestServer(t)

	var d Deploy
	if code := getJSON(t, ts.URL+"/api/deploy", &d); code != http.StatusOK {
		t.Fatalf("no release: status code %d", code)
	}
	if d.Active != "" {
		t.Errorf("no release: active = %q, want empty", d.Active)
	}

	// mc-agent points current at the active release's folder.
	release := filepath.Join(apps, "releases", "r7")
	if err := os.MkdirAll(release, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(release, filepath.Join(apps, "current")); err != nil {
		t.Fatal(err)
	}
	if code := getJSON(t, ts.URL+"/api/deploy", &d); code != http.StatusOK {
		t.Fatalf("r7 active: status code %d", code)
	}
	if d.Active != "r7" {
		t.Errorf("active = %q, want r7", d.Active)
	}
}

func TestRouting(t *testing.T) {
	ts, _ := newTestServer(t)

	tests := []struct {
		method, path string
		want         int
	}{
		{"GET", "/", http.StatusOK},
		{"GET", "/shared/base.css", http.StatusOK},
		{"GET", "/missing.js", http.StatusNotFound},
		{"POST", "/api/status", http.StatusMethodNotAllowed},
		{"POST", "/api/deploy", http.StatusMethodNotAllowed},
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

func TestProtect(t *testing.T) {
	ts, _ := newTestServer(t)
	denyAll := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "forbidden", http.StatusForbidden)
		})
	}
	h := protect(ts.Config.Handler, denyAll)

	tests := []struct {
		method, path string
		want         int
	}{
		{"GET", "/healthz", http.StatusOK},
		{"POST", "/healthz", http.StatusForbidden},
		{"GET", "/", http.StatusForbidden},
		{"GET", "/api/status", http.StatusForbidden},
		{"GET", "/api/deploy", http.StatusForbidden},
		{"GET", "/shared/base.css", http.StatusForbidden},
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))
		if rec.Code != tt.want {
			t.Errorf("%s %s = %d, want %d", tt.method, tt.path, rec.Code, tt.want)
		}
	}
}

func TestAuthFromEnv(t *testing.T) {
	t.Setenv("ACCESS_TEAM_DOMAIN", "")
	t.Setenv("ACCESS_AUD", "")
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	rec := httptest.NewRecorder()
	authFromEnv("")(ok).ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "ACCESS_") {
		t.Errorf("no config: %d %q, want 503 without config details", rec.Code, rec.Body.String())
	}

	auth := authFromEnv("dev@example.com")
	var email string
	h := auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { email = access.Email(r.Context()) }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if email != "dev@example.com" {
		t.Errorf("-dev-user: handler saw %q, want dev@example.com", email)
	}
}

func TestHealthzChecksTheApp(t *testing.T) {
	ts, apps := newTestServer(t)
	allow := func(h http.Handler) http.Handler { return h }
	h := protect(ts.Config.Handler, allow)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Errorf("healthy: %d %q, want 200 ok", rec.Code, rec.Body.String())
	}

	// current isn't a link, so /api/deploy fails, and so does /healthz,
	// without saying why.
	if err := os.WriteFile(filepath.Join(apps, "current"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "current") {
		t.Errorf("broken deploy state: %d %q, want a bare 503", rec.Code, rec.Body.String())
	}
}

func TestTeamIssuer(t *testing.T) {
	for in, want := range map[string]string{
		"myteam.cloudflareaccess.com":           "https://myteam.cloudflareaccess.com",
		" https://myteam.cloudflareaccess.com/": "https://myteam.cloudflareaccess.com",
		"":                                      "",
		"http://myteam.cloudflareaccess.com":    "",
		"myteam.cloudflareaccess.com/path":      "",
		"user@myteam.cloudflareaccess.com":      "",
	} {
		got, ok := teamIssuer(in)
		if got != want || ok != (want != "") {
			t.Errorf("teamIssuer(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

// activate makes tag the active release in apps, as mc-agent would, and
// writes its notes and applied record if they're given.
func activate(t *testing.T, apps, tag, notes, applied string) {
	t.Helper()
	release := filepath.Join(apps, "releases", tag)
	if err := os.MkdirAll(filepath.Join(release, "deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(release, filepath.Join(apps, "current")); err != nil {
		t.Fatal(err)
	}
	if notes != "" {
		if err := os.WriteFile(filepath.Join(release, "deploy", "notes.json"), []byte(notes), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if applied != "" {
		if err := os.WriteFile(filepath.Join(apps, "active.json"), []byte(applied), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAPIDeployDetails(t *testing.T) {
	ts, apps := newTestServer(t)
	activate(t, apps, "r7",
		`{"tag":"r7","kind":"feature","released_at":"2026-10-31T22:17:02Z","changes":[
			{"pr":90,"title":"Ship release notes","kind":"feature"},
			{"pr":null,"title":"Direct push","kind":"other"}]}`,
		`{"tag":"r7","applied_at":"2026-10-31T22:20:00Z"}`)

	resp, err := http.Get(ts.URL + "/api/deploy")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"active":      "r7",
		"kind":        "feature",
		"released_at": "2026-10-31T22:17:02Z",
		"applied_at":  "2026-10-31T22:20:00Z",
		"changes": []any{
			map[string]any{"pr": float64(90), "title": "Ship release notes", "kind": "feature"},
			map[string]any{"title": "Direct push", "kind": "other"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("/api/deploy = %v, want %v", got, want)
	}
}

func TestAPIDeployWithoutDetails(t *testing.T) {
	tests := []struct {
		name, notes, applied string
	}{
		{"release from before notes", "", ""},
		{"broken files", "{not json", "{not json"},
		{"another release's notes and time", `{"tag":"r6","kind":"fix"}`, `{"tag":"r6","applied_at":"2026-10-31T22:20:00Z"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts, apps := newTestServer(t)
			activate(t, apps, "r7", tt.notes, tt.applied)

			resp, err := http.Get(ts.URL + "/api/deploy")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var got map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if want := map[string]any{"active": "r7"}; resp.StatusCode != http.StatusOK || !reflect.DeepEqual(got, want) {
				t.Errorf("/api/deploy = %d %v, want 200 %v", resp.StatusCode, got, want)
			}
		})
	}
}
