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
	if _, err := authFromEnv(""); err == nil || !strings.Contains(err.Error(), "ACCESS_TEAM_DOMAIN and ACCESS_AUD") {
		t.Errorf("no config: err = %v, want one naming both keys", err)
	}

	auth, err := authFromEnv("dev@example.com")
	if err != nil {
		t.Fatal(err)
	}
	var email string
	h := auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { email = access.Email(r.Context()) }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if email != "dev@example.com" {
		t.Errorf("-dev-user: handler saw %q, want dev@example.com", email)
	}
}
