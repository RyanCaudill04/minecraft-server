package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/deploy"
)

// install puts a release in apps as mc-agent would, following update
// policies if policy is set.
func install(t *testing.T, apps, tag string, policy bool) {
	t.Helper()
	dir := filepath.Join(apps, "releases", tag, "deploy")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := `{"units": []}`
	if policy {
		m = `{"units": [], "policy": 1}`
	}
	if err := os.WriteFile(filepath.Join(dir, "release.json"), []byte(m), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setCurrent(t *testing.T, apps, tag string) {
	t.Helper()
	os.Remove(filepath.Join(apps, "current"))
	if err := os.Symlink(filepath.Join("releases", tag), filepath.Join(apps, "current")); err != nil {
		t.Fatal(err)
	}
}

// signedIn is a test app with a verified user, whose agent starts are
// counted in starts.
func signedIn(starts *atomic.Int32) func(a *app) {
	return func(a *app) {
		a.user = func(*http.Request) string { return "me@example.com" }
		a.startAgent = func(context.Context) error { starts.Add(1); return nil }
	}
}

// post sends a same-origin JSON POST with the CSRF header, unless change
// says otherwise, and returns the status code and the error message.
func post(t *testing.T, ts *httptest.Server, path, body string, change func(r *http.Request)) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(CSRFHeader, "1")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if change != nil {
		change(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var e struct{ Error string }
	json.NewDecoder(resp.Body).Decode(&e)
	return resp.StatusCode, e.Error
}

// withBody replaces a request's body with s.
func withBody(s string) func(r *http.Request) {
	return func(r *http.Request) {
		r.Body = io.NopCloser(strings.NewReader(s))
		r.ContentLength = int64(len(s))
	}
}

func pending(t *testing.T, apps string) *deploy.Request {
	t.Helper()
	r, err := (&deploy.Deployer{Apps: apps}).PendingRequest()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestChangesAreLockedWithoutAUser(t *testing.T) {
	var starts atomic.Int32
	ts, a := newTestApp(t, func(a *app) {
		a.startAgent = func(context.Context) error { starts.Add(1); return nil }
	})
	apps := a.agent.Apps
	install(t, apps, "r1", true)
	setCurrent(t, apps, "r1")

	for _, c := range []struct{ path, body string }{
		{"/api/settings/all", `{"mode": "pinned", "tag": "r1"}`},
		{"/api/settings/all", `{"mode": "auto"}`},
		{"/api/pause", `{"paused": true}`},
	} {
		code, msg := post(t, ts, c.path, c.body, nil)
		if code != http.StatusForbidden || msg != lockedNoUser {
			t.Errorf("POST %s = %d %q, want 403 locked", c.path, code, msg)
		}
	}
	if r := pending(t, apps); r != nil {
		t.Errorf("a locked request reached mc-agent: %+v", r)
	}
	if n := starts.Load(); n != 0 {
		t.Errorf("mc-agent was started %d times", n)
	}

	var s Settings
	if code := getJSON(t, ts.URL+"/api/settings", &s); code != http.StatusOK {
		t.Fatalf("GET /api/settings = %d", code)
	}
	if s.CanChange || s.Locked != lockedNoUser {
		t.Errorf("settings = %+v, want locked", s)
	}
}

func TestChangesNeedASameOriginJSONRequest(t *testing.T) {
	var starts atomic.Int32
	ts, a := newTestApp(t, signedIn(&starts))
	install(t, a.agent.Apps, "r1", true)
	setCurrent(t, a.agent.Apps, "r1")

	for name, c := range map[string]struct {
		change func(r *http.Request)
		want   int
	}{
		"no CSRF header":  {func(r *http.Request) { r.Header.Del(CSRFHeader) }, http.StatusForbidden},
		"wrong header":    {func(r *http.Request) { r.Header.Set(CSRFHeader, "yes") }, http.StatusForbidden},
		"cross-site":      {func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, http.StatusForbidden},
		"same-site":       {func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") }, http.StatusForbidden},
		"foreign origin":  {func(r *http.Request) { r.Header.Del("Sec-Fetch-Site"); r.Header.Set("Origin", "https://evil.example") }, http.StatusForbidden},
		"form post":       {func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }, http.StatusUnsupportedMediaType},
		"text/plain":      {func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, http.StatusUnsupportedMediaType},
		"too big":         {withBody(`{"mode": "` + strings.Repeat("a", 5000) + `"}`), http.StatusBadRequest},
		"unknown field":   {withBody(`{"mode": "auto", "by": "x"}`), http.StatusBadRequest},
		"trailing data":   {withBody(`{"mode": "auto"} {}`), http.StatusBadRequest},
		"matching origin": {func(r *http.Request) { r.Header.Del("Sec-Fetch-Site"); r.Header.Set("Origin", "http://"+r.Host) }, http.StatusAccepted},
	} {
		t.Run(name, func(t *testing.T) {
			os.Remove(filepath.Join(a.agent.Apps, "request.json"))
			code, _ := post(t, ts, "/api/settings/all", `{"mode": "auto"}`, c.change)
			if code != c.want {
				t.Errorf("status %d, want %d", code, c.want)
			}
			if got := pending(t, a.agent.Apps) != nil; got != (c.want == http.StatusAccepted) {
				t.Errorf("request written = %v", got)
			}
		})
	}
}

func TestApplyARelease(t *testing.T) {
	var starts atomic.Int32
	ts, a := newTestApp(t, signedIn(&starts))
	apps := a.agent.Apps
	install(t, apps, "r1", false) // from before policies
	install(t, apps, "r2", true)
	install(t, apps, "r3", true)
	setCurrent(t, apps, "r3")

	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	if code, msg := post(t, ts, "/api/settings/all", `{"mode": "pinned", "tag": "r1"}`, nil); code != http.StatusBadRequest || !strings.Contains(msg, "before update policies") {
		t.Errorf("pinning r1 = %d %q, want 400: it predates policies", code, msg)
	}
	if code, msg := post(t, ts, "/api/settings/all", `{"mode": "pinned", "tag": "r9"}`, nil); code != http.StatusBadRequest {
		t.Errorf("pinning an unknown release = %d %q, want 400", code, msg)
	}
	if code, msg := post(t, ts, "/api/settings/all", `{"mode": "pinned", "tag": "../r2"}`, nil); code != http.StatusBadRequest {
		t.Errorf("pinning a bad tag = %d %q, want 400", code, msg)
	}
	if starts.Load() != 0 || pending(t, apps) != nil {
		t.Fatal("a refused pin reached mc-agent")
	}

	if code, msg := post(t, ts, "/api/settings/all", `{"mode": "pinned", "tag": "r2"}`, nil); code != http.StatusAccepted {
		t.Fatalf("pinning r2 = %d %q, want 202", code, msg)
	}
	r := pending(t, apps)
	if r == nil || r.Action != deploy.ActionPin || r.Tag != "r2" || r.By != "me@example.com" {
		t.Errorf("request = %+v, want a pin to r2 by me@example.com", r)
	}
	if starts.Load() != 1 {
		t.Errorf("mc-agent was started %d times, want once", starts.Load())
	}
	if want := "admin: audit: me@example.com: update policy for all: auto -> pinned r2"; !strings.Contains(logs.String(), want) {
		t.Errorf("log = %q, want %q", logs.String(), want)
	}
	// admin only asks; mc-agent switches.
	if cur, _ := a.agent.Current(); cur != "r3" {
		t.Errorf("admin switched current to %s", cur)
	}

	// One change at a time.
	if code, _ := post(t, ts, "/api/settings/all", `{"mode": "auto"}`, nil); code != http.StatusConflict {
		t.Errorf("a second change = %d, want 409", code)
	}
	var s Settings
	getJSON(t, ts.URL+"/api/settings", &s)
	if s.Pending == nil || s.Pending.Tag != "r2" {
		t.Errorf("settings pending = %+v, want the pin to r2", s.Pending)
	}
	if b, _ := json.Marshal(s); strings.Contains(string(b), "me@example.com") {
		t.Errorf("GET /api/settings shows who asked: %s", b)
	}
}

func TestPolicyRequests(t *testing.T) {
	var starts atomic.Int32
	ts, a := newTestApp(t, signedIn(&starts))
	setCurrent(t, a.agent.Apps, "r1")
	install(t, a.agent.Apps, "r1", true)

	for _, c := range []struct {
		path, body string
		want       int
		action     string
	}{
		{"/api/settings/all", `{"mode": "notify"}`, http.StatusAccepted, deploy.ActionNotify},
		{"/api/settings/all", `{"mode": "auto"}`, http.StatusAccepted, deploy.ActionAuto},
		{"/api/pause", `{"paused": true}`, http.StatusAccepted, deploy.ActionPause},
		{"/api/pause", `{"paused": false}`, http.StatusAccepted, deploy.ActionResume},
		{"/api/pause", `{}`, http.StatusBadRequest, ""},
		{"/api/settings/play", `{"mode": "auto"}`, http.StatusNotFound, ""},
		{"/api/settings/all", `{"mode": "sometimes"}`, http.StatusBadRequest, ""},
		{"/api/settings/all", `{"mode": "auto", "tag": "r1"}`, http.StatusBadRequest, ""},
	} {
		os.Remove(filepath.Join(a.agent.Apps, "request.json"))
		code, msg := post(t, ts, c.path, c.body, nil)
		if code != c.want {
			t.Errorf("POST %s %s = %d %q, want %d", c.path, c.body, code, msg, c.want)
			continue
		}
		r := pending(t, a.agent.Apps)
		if c.action == "" {
			if r != nil {
				t.Errorf("POST %s %s wrote %+v", c.path, c.body, r)
			}
		} else if r == nil || r.Action != c.action {
			t.Errorf("POST %s %s wrote %+v, want %s", c.path, c.body, r, c.action)
		}
	}
}

func TestStartFailureStillQueues(t *testing.T) {
	ts, a := newTestApp(t, func(a *app) {
		a.user = func(*http.Request) string { return "me@example.com" }
		a.startAgent = func(context.Context) error { return fmt.Errorf("no systemd") }
	})
	setCurrent(t, a.agent.Apps, "r1")
	if code, _ := post(t, ts, "/api/pause", `{"paused": true}`, nil); code != http.StatusAccepted {
		t.Errorf("status %d, want 202: the timer picks it up", code)
	}
	if pending(t, a.agent.Apps) == nil {
		t.Error("the request wasn't queued")
	}
}

func TestSettingsShowThePolicy(t *testing.T) {
	ts, a := newTestApp(t, nil)
	apps := a.agent.Apps
	install(t, apps, "r1", true)
	setCurrent(t, apps, "r1")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(apps, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("policy.json", `{"version": 1, "paused": true, "tracks": {"all": {"mode": "notify"}}}`)
	write("latest.json", `{"tag": "r2", "checked_at": "2026-11-01T10:00:00Z"}`)
	write("request-result.json", `{"action": "pin", "tag": "r3", "by": "me@example.com", "at": "2026-11-01T09:00:00Z", "done_at": "2026-11-01T09:01:00Z", "error": "r3 failed"}`)

	var s Settings
	if code := getJSON(t, ts.URL+"/api/settings", &s); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	want := TrackSettings{Track: "all", Name: "All apps", Active: "r1", Available: "r2",
		CheckedAt: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC), Mode: deploy.ModeNotify}
	if len(s.Tracks) != 1 || s.Tracks[0] != want || !s.Paused {
		t.Errorf("settings = %+v, want paused and %+v", s, want)
	}
	if s.Last == nil || s.Last.Error != "r3 failed" || s.Last.Tag != "r3" {
		t.Errorf("last = %+v, want the failed pin to r3", s.Last)
	}

	write("pinned", "r1\n")
	getJSON(t, ts.URL+"/api/settings", &s)
	if got := s.Tracks[0]; got.Mode != deploy.ModePinned || got.Tag != "r1" {
		t.Errorf("track = %+v, want pinned to r1", got)
	}
}

func TestNewerPolicyIsReadOnly(t *testing.T) {
	var starts atomic.Int32
	ts, a := newTestApp(t, signedIn(&starts))
	install(t, a.agent.Apps, "r1", true)
	setCurrent(t, a.agent.Apps, "r1")
	os.WriteFile(filepath.Join(a.agent.Apps, "policy.json"), []byte(`{"version": 2}`), 0o644)

	var s Settings
	if code := getJSON(t, ts.URL+"/api/settings", &s); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if s.CanChange || s.Locked != lockedPolicy {
		t.Errorf("settings = %+v, want read-only", s)
	}
	if code, _ := post(t, ts, "/api/settings/all", `{"mode": "auto"}`, nil); code != http.StatusConflict {
		t.Errorf("POST = %d, want 409", code)
	}
	if pending(t, a.agent.Apps) != nil {
		t.Error("a request was written")
	}
}

func TestReleaseList(t *testing.T) {
	var calls atomic.Int32
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, `[
			{"tag_name": "r5", "published_at": "2026-10-05T00:00:00Z"},
			{"tag_name": "r4", "published_at": "2026-10-04T00:00:00Z"},
			{"tag_name": "r3", "published_at": "2026-10-03T00:00:00Z"},
			{"tag_name": "r2", "published_at": "2026-10-02T00:00:00Z"},
			{"tag_name": "r1", "published_at": "2026-10-01T00:00:00Z"}]`)
	}))
	t.Cleanup(gh.Close)
	ts, a := newTestApp(t, func(a *app) { a.agent.API = gh.URL })
	apps := a.agent.Apps
	install(t, apps, "r1", false)
	install(t, apps, "r3", true)
	install(t, apps, "r4", true)
	install(t, apps, "r0", false) // not on GitHub's list
	setCurrent(t, apps, "r4")
	os.WriteFile(filepath.Join(apps, "pinned"), []byte("r4\n"), 0o644)
	os.WriteFile(filepath.Join(apps, "failed"), []byte("r3\n"), 0o644)

	var l ReleaseList
	if code := getJSON(t, ts.URL+"/api/releases", &l); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	type view struct {
		installed, active, pinned, failed, selectable bool
	}
	want := map[string]view{
		"r5": {selectable: true},              // newer than r3, the first with policies
		"r4": {true, true, true, false, true}, // active and pinned
		"r3": {true, false, false, true, true},
		"r2": {}, // older than r3 and not installed: can't tell, so no
		"r1": {installed: true},
		"r0": {installed: true},
	}
	var order []string
	for _, r := range l.Releases {
		order = append(order, r.Tag)
		got := view{r.Installed, r.Active, r.Pinned, r.Failed, r.Selectable}
		if got != want[r.Tag] {
			t.Errorf("%s = %+v, want %+v", r.Tag, got, want[r.Tag])
		}
		if !r.Selectable && r.Reason == "" {
			t.Errorf("%s isn't selectable but says no reason", r.Tag)
		}
		if r.URL != "https://github.com/o/r/releases/tag/"+r.Tag {
			t.Errorf("%s url = %q", r.Tag, r.URL)
		}
	}
	if got := strings.Join(order, " "); got != "r5 r4 r3 r2 r1 r0" {
		t.Errorf("order = %s", got)
	}
	// Cached: GitHub's limit is 60 an hour.
	getJSON(t, ts.URL+"/api/releases", &l)
	if n := calls.Load(); n != 1 {
		t.Errorf("GitHub was asked %d times, want once", n)
	}
}

func TestReleaseListWithoutGitHub(t *testing.T) {
	ts, a := newTestApp(t, nil)
	install(t, a.agent.Apps, "r1", true)
	setCurrent(t, a.agent.Apps, "r1")
	var l ReleaseList
	if code := getJSON(t, ts.URL+"/api/releases", &l); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if l.GitHubError == "" || len(l.Releases) != 1 || !l.Releases[0].Selectable {
		t.Errorf("list = %+v, want the installed r1 and GitHub's error", l)
	}
}
