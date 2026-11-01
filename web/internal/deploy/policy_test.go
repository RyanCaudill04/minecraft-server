package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func (h *harness) run(t *testing.T) {
	t.Helper()
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) mode(t *testing.T) TrackPolicy {
	t.Helper()
	p, err := h.d.ReadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	return p.Tracks[Track]
}

func TestNotifyRecordsButDoesntInstall(t *testing.T) {
	h := newHarness(t)
	h.deployAll(t, "r1")
	if err := h.d.SetMode(ModeNotify); err != nil {
		t.Fatal(err)
	}
	h.publish(t, "r2", "/ok")
	h.run(t)

	if got := h.current(t); got != "r1" {
		t.Errorf("current = %q, want r1 kept in notify mode", got)
	}
	if h.exists("releases/r2") {
		t.Error("notify mode installed r2")
	}
	if calls := h.takeCalls(); len(calls) != 0 {
		t.Errorf("notify mode called systemctl: %q", calls)
	}
	seen, err := h.d.LatestSeen()
	if err != nil || seen == nil || seen.Tag != "r2" {
		t.Errorf("LatestSeen = %+v, %v; want r2 recorded", seen, err)
	}

	if err := h.d.SetMode(ModeAuto); err != nil {
		t.Fatal(err)
	}
	h.run(t)
	if got := h.current(t); got != "r2" {
		t.Errorf("current = %q after switching to auto, want r2", got)
	}
}

func TestPauseHoldsEverything(t *testing.T) {
	h := newHarness(t)
	h.deployAll(t, "r1")
	if err := h.d.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	h.publish(t, "r2", "/ok")
	// GitHub isn't asked while paused.
	h.github.Close()
	h.run(t)
	if got := h.current(t); got != "r1" {
		t.Errorf("current = %q while paused, want r1", got)
	}
	if !h.policy(t).Paused {
		t.Error("ReadPolicy doesn't say paused")
	}
	if err := h.d.SetPaused(false); err != nil {
		t.Fatal(err)
	}
	if h.policy(t).Paused {
		t.Error("still paused after resuming")
	}
}

func (h *harness) policy(t *testing.T) *Policy {
	t.Helper()
	p, err := h.d.ReadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPinStaysUntilFollowLatest(t *testing.T) {
	h := newHarness(t)
	h.d.Keep = 5
	h.deployAll(t, "r1", "r2", "r3")

	if err := h.d.Pin(context.Background(), "r1"); err != nil {
		t.Fatal(err)
	}
	if got := h.current(t); got != "r1" {
		t.Fatalf("current = %q, want r1", got)
	}
	if got, want := h.mode(t), (TrackPolicy{ModePinned, "r1"}); got != want {
		t.Errorf("policy = %+v, want %+v", got, want)
	}
	h.publish(t, "r4", "/ok")
	h.takeCalls()
	for range 3 {
		h.run(t)
	}
	if got := h.current(t); got != "r1" {
		t.Errorf("current = %q after timer runs, want the pin to r1 kept", got)
	}
	if calls := h.takeCalls(); len(calls) != 0 {
		t.Errorf("pinned runs called systemctl: %q", calls)
	}

	if err := h.d.SetMode(ModeAuto); err != nil {
		t.Fatal(err)
	}
	h.run(t)
	if got := h.current(t); got != "r4" {
		t.Errorf("current = %q after following latest, want r4", got)
	}
	if h.exists("pinned") {
		t.Error("following latest left the pin")
	}
}

func TestPinInstallsAReleaseFromGitHub(t *testing.T) {
	h := newHarness(t)
	h.publish(t, "r1", "/ok") // published but never installed
	checked := 0
	h.d.CheckAgent = func(ctx context.Context) error { checked++; return nil }
	h.deployAll(t, "r2")
	checked = 0

	if err := h.d.Pin(context.Background(), "r1"); err != nil {
		t.Fatal(err)
	}
	if got := h.current(t); got != "r1" {
		t.Errorf("current = %q, want r1", got)
	}
	if checked != 1 {
		t.Errorf("r1's agent was checked %d times, want once: it never passed a deploy here", checked)
	}

	if err := h.d.Pin(context.Background(), "r9"); err == nil {
		t.Error("want an error pinning a release GitHub doesn't have")
	}
	if got := h.mode(t); got.Tag != "r1" {
		t.Errorf("a failed lookup changed the pin: %+v", got)
	}
}

func TestFailedPinSwitchesToNotify(t *testing.T) {
	h := newHarness(t)
	h.d.HealthTimeout = 300 * time.Millisecond
	h.deployAll(t, "r1")
	h.publish(t, "r2", "/bad")
	if err := h.d.SetMode(ModeNotify); err != nil {
		t.Fatal(err)
	}
	if err := h.d.SetMode(ModeAuto); err != nil {
		t.Fatal(err)
	}

	err := h.d.Pin(context.Background(), "r2")
	if err == nil || !strings.Contains(err.Error(), "rolled back to r1") {
		t.Fatalf("err = %v, want r2 rolled back to r1", err)
	}
	if got := h.current(t); got != "r1" {
		t.Errorf("current = %q, want r1", got)
	}
	if got, want := h.mode(t), (TrackPolicy{Mode: ModeNotify}); got != want {
		t.Errorf("policy = %+v, want %+v", got, want)
	}
	h.takeCalls()
	for range 2 {
		h.run(t)
	}
	if calls := h.takeCalls(); len(calls) != 0 {
		t.Errorf("r2 was retried: %q", calls)
	}
}

func TestPinRefusesReleasesFromBeforePolicies(t *testing.T) {
	h := newHarness(t)
	h.publishWith(t, "r1", "/ok", Manifest{
		Units: []string{"play.service", "mc-agent.timer"}, Services: []string{"play.service"}, Timers: []string{"mc-agent.timer"},
	})
	h.run(t)
	h.deployAll(t, "r2")

	err := h.d.Pin(context.Background(), "r1")
	if err == nil || !strings.Contains(err.Error(), "before update policies") {
		t.Fatalf("err = %v, want r1 refused", err)
	}
	if got := h.current(t); got != "r2" {
		t.Errorf("current = %q, want r2", got)
	}
	if h.exists("pinned") {
		t.Error("a refused release was pinned")
	}
	rels, err := h.d.InstalledReleases()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rels {
		got[r.Tag] = r.Policy
	}
	if want := map[string]bool{"r1": false, "r2": true}; !reflect.DeepEqual(got, want) {
		t.Errorf("InstalledReleases policy support = %v, want %v", got, want)
	}
}

func TestPinTheActiveRelease(t *testing.T) {
	h := newHarness(t)
	h.deployAll(t, "r1")
	if err := h.d.Pin(context.Background(), "r1"); err != nil {
		t.Fatal(err)
	}
	if calls := h.takeCalls(); len(calls) != 0 {
		t.Errorf("pinning the active release called systemctl: %q", calls)
	}
	h.publish(t, "r2", "/ok")
	h.run(t)
	if got := h.current(t); got != "r1" {
		t.Errorf("current = %q, want r1", got)
	}
}

func TestUnsupportedPolicyIsIgnoredAndLeftAlone(t *testing.T) {
	h := newHarness(t)
	h.deployAll(t, "r1")
	newer := []byte(`{"version": 99, "paused": true, "tracks": {"all": {"mode": "notify"}}}`)
	if err := os.WriteFile(filepath.Join(h.d.Apps, "policy.json"), newer, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.ReadPolicy(); !errors.Is(err, ErrUnsupportedPolicy) {
		t.Errorf("ReadPolicy err = %v, want ErrUnsupportedPolicy", err)
	}
	if err := h.d.SetMode(ModeAuto); !errors.Is(err, ErrUnsupportedPolicy) {
		t.Errorf("SetMode err = %v, want ErrUnsupportedPolicy", err)
	}
	// Deploys go on, so a fix can still arrive.
	h.publish(t, "r2", "/ok")
	h.run(t)
	if got := h.current(t); got != "r2" {
		t.Errorf("current = %q, want r2", got)
	}
	if data, _ := os.ReadFile(filepath.Join(h.d.Apps, "policy.json")); string(data) != string(newer) {
		t.Errorf("policy.json was rewritten: %s", data)
	}
}

func TestSetModeRejectsPinned(t *testing.T) {
	h := newHarness(t)
	if err := h.d.SetMode(ModePinned); err == nil {
		t.Error("want an error: pinning needs a tag")
	}
	if err := h.d.SetMode("sometimes"); err == nil {
		t.Error("want an error for an unknown mode")
	}
}

func request(action, tag string) Request {
	return Request{Action: action, Track: Track, Tag: tag, By: "me@example.com", At: time.Now()}
}

func TestRequestsAreCarriedOutByTheNextRun(t *testing.T) {
	h := newHarness(t)
	h.d.Keep = 5
	h.deployAll(t, "r1", "r2")

	if err := h.d.Submit(request(ActionPin, "r1")); err != nil {
		t.Fatal(err)
	}
	if p, err := h.d.PendingRequest(); err != nil || p == nil || p.Tag != "r1" {
		t.Errorf("PendingRequest = %+v, %v; want the pin to r1", p, err)
	}
	if err := h.d.Submit(request(ActionAuto, "")); !errors.Is(err, ErrBusy) {
		t.Errorf("a second request: err = %v, want ErrBusy", err)
	}
	h.run(t)
	if got := h.current(t); got != "r1" {
		t.Errorf("current = %q, want r1", got)
	}
	if h.exists("request.json") {
		t.Error("the request is still waiting")
	}
	res, err := h.d.LastRequest()
	if err != nil || res == nil || res.Tag != "r1" || res.By != "me@example.com" || res.Error != "" || res.Done.IsZero() {
		t.Errorf("LastRequest = %+v, %v; want r1 done without an error", res, err)
	}

	for _, step := range []struct {
		action string
		check  func() bool
	}{
		{ActionPause, func() bool { return h.policy(t).Paused }},
		{ActionResume, func() bool { return !h.policy(t).Paused && h.current(t) == "r1" }},
		{ActionNotify, func() bool { return h.mode(t).Mode == ModeNotify && h.current(t) == "r1" }},
		{ActionAuto, func() bool { return h.mode(t).Mode == ModeAuto && h.current(t) == "r2" }},
	} {
		if err := h.d.Submit(request(step.action, "")); err != nil {
			t.Fatal(err)
		}
		h.run(t)
		if !step.check() {
			t.Errorf("after %s: policy %+v, current %s", step.action, h.policy(t), h.current(t))
		}
	}
}

func TestFailedRequestIsRecorded(t *testing.T) {
	h := newHarness(t)
	h.deployAll(t, "r1")
	if err := h.d.Submit(request(ActionPin, "r9")); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Run(context.Background()); err == nil {
		t.Error("want the run to fail: GitHub has no r9")
	}
	res, err := h.d.LastRequest()
	if err != nil || res == nil || res.Error == "" {
		t.Errorf("LastRequest = %+v, %v; want the error recorded", res, err)
	}
	// It isn't retried.
	h.run(t)
}

func TestBadAndExpiredRequestsAreDropped(t *testing.T) {
	for name, body := range map[string]string{
		"garbage": `{`,
		"action":  fmt.Sprintf(`{"action": "rm", "by": "me@example.com", "at": %q}`, time.Now().Format(time.RFC3339)),
		"expired": fmt.Sprintf(`{"action": "pause", "by": "me@example.com", "at": %q}`, time.Now().Add(-time.Hour).Format(time.RFC3339)),
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.deployAll(t, "r1")
			if err := os.WriteFile(filepath.Join(h.d.Apps, "request.json"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			h.run(t)
			if h.exists("request.json") {
				t.Error("the request wasn't dropped")
			}
			if h.policy(t).Paused {
				t.Error("an expired request was carried out")
			}
			if res, _ := h.d.LastRequest(); res == nil || res.Error == "" {
				t.Errorf("LastRequest = %+v, want the reason recorded", res)
			}
		})
	}
}

func TestExpiredRequestCanBeReplaced(t *testing.T) {
	h := newHarness(t)
	old := request(ActionPause, "")
	old.At = time.Now().Add(-time.Hour)
	if err := h.d.Submit(old); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Submit(request(ActionNotify, "")); err != nil {
		t.Errorf("replacing an expired request: %v", err)
	}
}

func TestRequestCheck(t *testing.T) {
	ok := request(ActionPin, "r1")
	if err := ok.Check(); err != nil {
		t.Fatalf("a good request: %v", err)
	}
	for name, change := range map[string]func(r *Request){
		"unknown action":   func(r *Request) { r.Action = "deploy" },
		"pin without tag":  func(r *Request) { r.Tag = "" },
		"bad tag":          func(r *Request) { r.Tag = "../r1" },
		"flag tag":         func(r *Request) { r.Tag = "-r1" },
		"tag on auto":      func(r *Request) { r.Action = ActionAuto },
		"unknown track":    func(r *Request) { r.Track = "play" },
		"no user":          func(r *Request) { r.By = "" },
		"newline in user":  func(r *Request) { r.By = "a@b.c\nforged" },
		"space in user":    func(r *Request) { r.By = "a b@c.d" },
		"bidi in user":     func(r *Request) { r.By = "a‮@b.c" },
		"no time":          func(r *Request) { r.At = time.Time{} },
		"long user":        func(r *Request) { r.By = strings.Repeat("a", 255) },
		"line sep in user": func(r *Request) { r.By = "a @b.c" },
	} {
		r := ok
		change(&r)
		if err := r.Check(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestReleasesSkipsDraftsAndPrereleases(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/releases" || r.URL.Query().Get("per_page") == "" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `[
			{"tag_name": "r4", "draft": true},
			{"tag_name": "r3", "prerelease": true},
			{"tag_name": "r2", "published_at": "2026-10-02T00:00:00Z", "html_url": "https://github.com/o/r/releases/tag/r2"},
			{"tag_name": "bad tag"},
			{"tag_name": "r1", "published_at": "2026-10-01T00:00:00Z"}]`)
	}))
	defer gh.Close()
	d := &Deployer{Repo: "o/r", API: gh.URL, HTTP: http.DefaultClient, Log: log.New(io.Discard, "", 0)}
	rels, err := d.Releases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var tags []string
	for _, r := range rels {
		tags = append(tags, r.Tag)
	}
	if want := []string{"r2", "r1"}; !reflect.DeepEqual(tags, want) {
		t.Errorf("tags = %q, want %q", tags, want)
	}
	if rels[0].URL == "" || rels[0].PublishedAt.IsZero() {
		t.Errorf("r2 = %+v, want its page and date", rels[0])
	}
}

func TestReadsCreateNothing(t *testing.T) {
	d := &Deployer{Apps: filepath.Join(t.TempDir(), "apps"), Log: log.New(io.Discard, "", 0)}
	if _, err := d.ReadPolicy(); err != nil {
		t.Error(err)
	}
	if _, err := d.InstalledReleases(); err != nil {
		t.Error(err)
	}
	if _, err := d.LatestSeen(); err != nil {
		t.Error(err)
	}
	if _, err := d.PendingRequest(); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(d.Apps); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("reading created %s", d.Apps)
	}
}
