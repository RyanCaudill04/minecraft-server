package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/deploy/state"
)

// trackUnits is what each test track ships. "" is the single release,
// which ships all of them.
var trackUnits = map[string]Manifest{
	"play":  {Units: []string{"play.service"}, Services: []string{"play.service"}},
	"admin": {Units: []string{"admin.service"}, Services: []string{"admin.service"}},
	"agent": {Units: []string{"mc-agent.service", "mc-agent.timer"}, Timers: []string{"mc-agent.timer"}},
}

// trackFiles is a release of track (or of the single release, for "")
// whose health check is healthURL.
func trackFiles(t *testing.T, track, healthURL string) []entry {
	t.Helper()
	m := trackUnits[track]
	if track == "" {
		m = *union(ptr(trackUnits["play"]), ptr(trackUnits["admin"]), ptr(trackUnits["agent"]))
	}
	m.Track = track
	if len(m.Services) > 0 {
		m.Health = []string{healthURL}
	}
	manifest, _ := json.Marshal(m)
	files := []entry{{name: ManifestPath, body: string(manifest)}, {name: "bin/mc-agent", body: "#!/bin/sh\n", mode: 0o755}}
	for _, u := range m.Units {
		files = append(files, entry{name: "deploy/systemd/" + u, body: "[Unit]\n"})
	}
	return files
}

func ptr[T any](v T) *T { return &v }

type agentHarness struct {
	*harness
	a *Agent

	checkMu   sync.Mutex
	checked   []string // bin paths CheckAgent ran
	checkFail bool
}

func newAgentHarness(t *testing.T) *agentHarness {
	h := &agentHarness{harness: newHarness(t)}
	h.d.HealthTimeout = 300 * time.Millisecond
	h.d.AgentTimer = "mc-agent.timer"
	h.a = &Agent{
		Apps:    h.d.Apps,
		Repo:    "o/r",
		API:     h.github.URL,
		UnitDir: h.d.UnitDir,
		// The agent is listed first; it must still be deployed last.
		Tracks:        []Track{{Name: "agent"}, {Name: "play"}, {Name: "admin"}},
		AgentTrack:    "agent",
		AgentTimer:    "mc-agent.timer",
		HTTP:          http.DefaultClient,
		Systemctl:     h.d.Systemctl,
		HealthTimeout: 300 * time.Millisecond,
		Keep:          3,
		Log:           log.New(io.Discard, "", 0),
	}
	h.a.CheckAgent = func(ctx context.Context, bin string) error {
		h.checkMu.Lock()
		defer h.checkMu.Unlock()
		h.checked = append(h.checked, bin)
		if h.checkFail {
			return errors.New("selfcheck failed")
		}
		return nil
	}
	return h
}

func (h *agentHarness) pub(t *testing.T, tag, track, healthPath string) {
	t.Helper()
	h.github.publish(tag, tarball(t, trackFiles(t, track, h.health.URL+healthPath)))
}

// onSingle has the old agent deploy the single release r1, as on the Pi
// today, then publishes the first release of every track.
func (h *agentHarness) onSingle(t *testing.T) {
	t.Helper()
	h.pub(t, "r1", "", "/ok")
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.pub(t, "play-r1", "play", "/ok")
	h.pub(t, "admin-r1", "admin", "/ok")
	h.pub(t, "agent-r1", "agent", "/ok")
	h.takeCalls()
}

func (h *agentHarness) layout(t *testing.T) state.Layout {
	t.Helper()
	st, err := state.Read(h.a.Apps)
	if err != nil {
		t.Fatal(err)
	}
	return st.Layout
}

func (h *agentHarness) active(t *testing.T, track string) string {
	t.Helper()
	d, err := h.a.TrackDeployer(track)
	if err != nil {
		t.Fatal(err)
	}
	tag, err := d.Current()
	if err != nil {
		t.Fatal(err)
	}
	return tag
}

// linkDir returns the folder whose current release unit links to.
func (h *agentHarness) linkDir(t *testing.T, unit string) string {
	t.Helper()
	target, err := os.Readlink(filepath.Join(h.a.UnitDir, unit))
	if err != nil {
		t.Fatal(err)
	}
	rel, _ := filepath.Rel(h.a.Apps, target)
	dir, _, _ := strings.Cut(rel, string(filepath.Separator)+"deploy"+string(filepath.Separator))
	return dir
}

func TestMigrateFromSingle(t *testing.T) {
	tests := []struct {
		name      string
		badHealth string // track whose first release fails its health check
		badAgent  bool   // the agent's selfcheck fails
		wantCalls []string
	}{
		{
			name: "every track healthy",
			// One track at a time, the agent last.
			wantCalls: []string{
				"daemon-reload", "enable play.service", "restart play.service",
				"daemon-reload", "enable admin.service", "restart admin.service",
				"daemon-reload", "enable --now mc-agent.timer",
			},
		},
		{
			name:      "play fails",
			badHealth: "play",
			wantCalls: []string{
				"daemon-reload", "enable play.service", "restart play.service",
				// Back on r1: all of it restarted.
				"daemon-reload", "enable play.service admin.service", "restart play.service admin.service", "enable --now mc-agent.timer",
			},
		},
		{
			name:      "admin fails after play moved",
			badHealth: "admin",
			wantCalls: []string{
				"daemon-reload", "enable play.service", "restart play.service",
				"daemon-reload", "enable admin.service", "restart admin.service",
				"daemon-reload", "enable play.service admin.service", "restart play.service admin.service", "enable --now mc-agent.timer",
			},
		},
		{
			name:     "agent fails its selfcheck",
			badAgent: true,
			wantCalls: []string{
				"daemon-reload", "enable play.service", "restart play.service",
				"daemon-reload", "enable admin.service", "restart admin.service",
				"daemon-reload", "enable --now mc-agent.timer",
				"daemon-reload", "enable play.service admin.service", "restart play.service admin.service", "enable --now mc-agent.timer",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newAgentHarness(t)
			h.pub(t, "r1", "", "/ok")
			if err := h.d.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			for _, track := range []string{"play", "admin", "agent"} {
				path := "/ok"
				if track == tt.badHealth {
					path = "/bad"
				}
				h.pub(t, track+"-r1", track, path)
			}
			h.checkFail = tt.badAgent
			h.takeCalls()

			err := h.a.Run(context.Background())
			if got := h.takeCalls(); !reflect.DeepEqual(got, tt.wantCalls) {
				t.Errorf("systemctl calls:\n got %q\nwant %q", got, tt.wantCalls)
			}
			failed := tt.badHealth != "" || tt.badAgent
			if failed != (err != nil) {
				t.Fatalf("err = %v, want failure %v", err, failed)
			}

			units := []string{"play.service", "admin.service", "mc-agent.service", "mc-agent.timer"}
			if !failed {
				if got := h.layout(t); got != state.Tracks {
					t.Errorf("layout = %q, want tracks", got)
				}
				for _, u := range units {
					want := map[string]string{"play.service": "play", "admin.service": "admin"}[u]
					if want == "" {
						want = "agent"
					}
					if got := h.linkDir(t, u); got != filepath.Join(want, "current") {
						t.Errorf("%s links into %s, want %s/current", u, got, want)
					}
				}
				if want := []string{filepath.Join(h.a.Apps, "agent", "current", "bin", "mc-agent")}; !reflect.DeepEqual(h.checked, want) {
					t.Errorf("selfcheck ran %q, want %q", h.checked, want)
				}
				// The single release stays until the next run.
				if _, err := os.Lstat(filepath.Join(h.a.Apps, "current")); err != nil {
					t.Errorf("the single release was removed in the same run: %v", err)
				}
				return
			}

			// Everything is back on the single release.
			if got := h.layout(t); got != state.Single {
				t.Errorf("layout = %q, want single", got)
			}
			for _, u := range units {
				if got := h.linkDir(t, u); got != "current" {
					t.Errorf("%s links into %s, want the single release's current", u, got)
				}
			}
			if got := h.active(t, ""); got != "r1" {
				t.Errorf("single release = %q, want r1", got)
			}
			for _, track := range []string{"play", "admin", "agent"} {
				if got := h.active(t, track); got != "" {
					t.Errorf("%s still has %s active after the move was undone", track, got)
				}
			}
			st, _ := state.Read(h.a.Apps)
			if st.Migration == nil || st.Migration.Tags["play"] != "play-r1" {
				t.Errorf("migration = %+v, want the failed attempt recorded", st.Migration)
			}

			// The next run doesn't try the same releases again.
			if err := h.a.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if calls := h.takeCalls(); len(calls) != 0 {
				t.Errorf("a retry of the failed move called systemctl: %q", calls)
			}

			// A fixed release of the failing track lets it go through.
			h.checkFail = false
			if tt.badAgent {
				h.pub(t, "agent-r2", "agent", "/ok")
			} else {
				h.pub(t, tt.badHealth+"-r2", tt.badHealth, "/ok")
			}
			if err := h.a.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := h.layout(t); got != state.Tracks {
				t.Errorf("after a fixed release: layout = %q, want tracks", got)
			}
		})
	}
}

func TestMigrationRemovesSingleOnNextRun(t *testing.T) {
	h := newAgentHarness(t)
	h.onSingle(t)
	os.WriteFile(filepath.Join(h.a.Apps, "failed"), []byte("r0\n"), 0o644)
	if err := h.a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.takeCalls()

	if err := h.a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls := h.takeCalls(); len(calls) != 0 {
		t.Errorf("an up-to-date run called systemctl: %q", calls)
	}
	for _, name := range []string{"current", "releases", "failed", "managed.json", "active.json", "history"} {
		if _, err := os.Lstat(filepath.Join(h.a.Apps, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still there: %v", name, err)
		}
	}
	for _, track := range []string{"play", "admin", "agent"} {
		if got := h.active(t, track); got != track+"-r1" {
			t.Errorf("%s = %q, want %s-r1", track, got, track)
		}
	}
}

// A unit that still links to the single release keeps it in place.
func TestSingleKeptWhileLinked(t *testing.T) {
	h := newAgentHarness(t)
	h.onSingle(t)
	if err := h.a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(h.a.UnitDir, "stray.service")
	os.Symlink(filepath.Join(h.a.Apps, "current", "deploy", "systemd", "play.service"), stray)
	if err := h.a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(h.a.Apps, "current")); err != nil {
		t.Errorf("the single release was removed while %s links to it", stray)
	}
}

// The agent is killed partway through the move. The next run finishes it.
func TestMigrationInterrupted(t *testing.T) {
	h := newAgentHarness(t)
	h.onSingle(t)
	h.failWhen = func(_ string, args []string) bool {
		if slices.Equal(args, []string{"restart", "admin.service"}) {
			runtime.Goexit() // the agent dies here
		}
		return false
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.a.Run(context.Background())
	}()
	<-done
	if got := h.layout(t); got != state.Single {
		t.Fatalf("layout = %q after a killed move, want single", got)
	}
	h.failWhen = nil
	h.takeCalls()

	if err := h.a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.layout(t); got != state.Tracks {
		t.Errorf("layout = %q, want tracks", got)
	}
	for _, u := range []string{"play.service", "admin.service"} {
		if got := h.linkDir(t, u); got == "current" {
			t.Errorf("%s still links to the single release", u)
		}
	}
}

func TestWaitsForEveryTrack(t *testing.T) {
	h := newAgentHarness(t)
	h.pub(t, "r1", "", "/ok")
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.pub(t, "play-r1", "play", "/ok")
	h.pub(t, "agent-r1", "agent", "/ok")
	h.pub(t, "r2", "", "/ok")

	// admin has no release yet, so it stays on the single release and
	// deploys its newest one.
	if err := h.a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.layout(t); got != state.Single {
		t.Errorf("layout = %q, want single", got)
	}
	if got := h.active(t, ""); got != "r2" {
		t.Errorf("single release = %q, want r2", got)
	}
	if got := h.active(t, "play"); got != "" {
		t.Errorf("play = %q, want nothing installed yet", got)
	}
}

func TestPinnedSingleDoesNotMigrate(t *testing.T) {
	h := newAgentHarness(t)
	h.onSingle(t)
	os.WriteFile(filepath.Join(h.a.Apps, "pinned"), []byte("r1\n"), 0o644)
	if err := h.a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.layout(t); got != state.Single {
		t.Errorf("layout = %q, want single while pinned", got)
	}
}

func TestFreshPiStartsOnTracks(t *testing.T) {
	h := newAgentHarness(t)
	h.pub(t, "play-r1", "play", "/ok")
	h.pub(t, "admin-r1", "admin", "/ok")
	h.pub(t, "agent-r1", "agent", "/ok")
	if err := h.a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.layout(t); got != state.Tracks {
		t.Errorf("layout = %q, want tracks", got)
	}
	for _, track := range []string{"play", "admin", "agent"} {
		if got := h.active(t, track); got != track+"-r1" {
			t.Errorf("%s = %q, want %s-r1", track, got, track)
		}
	}
}

// migrated returns a harness whose Pi has moved to tracks, on r1 of each.
func migrated(t *testing.T) *agentHarness {
	h := newAgentHarness(t)
	h.onSingle(t)
	for range 2 { // the second run removes the single release
		if err := h.a.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	h.takeCalls()
	return h
}

func TestTracksDeployIndependently(t *testing.T) {
	tests := []struct {
		name      string
		publish   map[string]string // track -> health path of its r2
		wantErr   string            // "" for success
		want      map[string]string // track -> active tag
		wantCalls []string
	}{
		{
			name:      "only play changed",
			publish:   map[string]string{"play": "/ok"},
			want:      map[string]string{"play": "play-r2", "admin": "admin-r1", "agent": "agent-r1"},
			wantCalls: []string{"daemon-reload", "enable play.service", "restart play.service"},
		},
		{
			name:    "two tracks changed",
			publish: map[string]string{"play": "/ok", "admin": "/ok"},
			want:    map[string]string{"play": "play-r2", "admin": "admin-r2", "agent": "agent-r1"},
			wantCalls: []string{
				"daemon-reload", "enable play.service", "restart play.service",
				"daemon-reload", "enable admin.service", "restart admin.service",
			},
		},
		{
			name:    "play fails, admin still updates",
			publish: map[string]string{"play": "/bad", "admin": "/ok"},
			wantErr: "play:",
			want:    map[string]string{"play": "play-r1", "admin": "admin-r2", "agent": "agent-r1"},
			wantCalls: []string{
				"daemon-reload", "enable play.service", "restart play.service",
				"daemon-reload", "enable play.service", "restart play.service", // back on play-r1
				"daemon-reload", "enable admin.service", "restart admin.service",
			},
		},
		{
			name:      "agent updates last",
			publish:   map[string]string{"agent": "/ok", "play": "/ok"},
			want:      map[string]string{"play": "play-r2", "admin": "admin-r1", "agent": "agent-r2"},
			wantCalls: []string{"daemon-reload", "enable play.service", "restart play.service", "daemon-reload", "enable --now mc-agent.timer"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := migrated(t)
			for track, path := range tt.publish {
				h.pub(t, track+"-r2", track, path)
			}
			err := h.a.Run(context.Background())
			if tt.wantErr == "" && err != nil || tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Errorf("err = %v, want %q", err, tt.wantErr)
			}
			for track, want := range tt.want {
				if got := h.active(t, track); got != want {
					t.Errorf("%s = %q, want %q", track, got, want)
				}
			}
			if got := h.takeCalls(); !reflect.DeepEqual(got, tt.wantCalls) {
				t.Errorf("systemctl calls:\n got %q\nwant %q", got, tt.wantCalls)
			}
			// A failed track isn't retried; the others stay put.
			if err := h.a.Run(context.Background()); err != nil {
				t.Errorf("second run: %v", err)
			}
			if calls := h.takeCalls(); len(calls) != 0 {
				t.Errorf("second run called systemctl: %q", calls)
			}
		})
	}
}

func TestTrackRollback(t *testing.T) {
	h := migrated(t)
	h.pub(t, "play-r2", "play", "/ok")
	h.pub(t, "admin-r2", "admin", "/ok")
	if err := h.a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.takeCalls()

	if err := h.a.Rollback(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "play") {
		t.Errorf("rollback without a track: err = %v, want one naming the tracks", err)
	}
	if err := h.a.Rollback(context.Background(), "nope"); err == nil {
		t.Error("rollback of an unknown track: want an error")
	}
	if err := h.a.Rollback(context.Background(), "play"); err != nil {
		t.Fatal(err)
	}
	if got := h.takeCalls(); !reflect.DeepEqual(got, []string{"daemon-reload", "enable play.service", "restart play.service"}) {
		t.Errorf("rollback restarted %q, want only play", got)
	}
	if got := h.active(t, "play"); got != "play-r1" {
		t.Errorf("play = %q, want play-r1", got)
	}
	if got := h.active(t, "admin"); got != "admin-r2" {
		t.Errorf("admin = %q, want admin-r2", got)
	}

	// The pin holds play back, but admin still follows its releases.
	h.pub(t, "admin-r3", "admin", "/ok")
	if err := h.a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := [2]string{h.active(t, "play"), h.active(t, "admin")}, [2]string{"play-r1", "admin-r3"}; got != want {
		t.Errorf("after a run: play, admin = %q, want %q", got, want)
	}
	if err := h.a.Unpin("play"); err != nil {
		t.Fatal(err)
	}
	if err := h.a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.active(t, "play"); got != "play-r2" {
		t.Errorf("after unpin: play = %q, want play-r2", got)
	}

	s, err := h.a.Status()
	if err != nil {
		t.Fatal(err)
	}
	if s.Layout != state.Tracks || len(s.Tracks) != 3 || s.Tracks[0].Track != "play" || s.Tracks[0].Active != "play-r2" {
		t.Errorf("status = %+v", s)
	}
}

func TestRollbackOnSingle(t *testing.T) {
	h := newAgentHarness(t)
	h.pub(t, "r1", "", "/ok")
	h.d.Run(context.Background())
	h.pub(t, "r2", "", "/ok")
	h.d.Run(context.Background())
	if err := h.a.Rollback(context.Background(), "play"); err == nil {
		t.Error("rollback of a track on the single layout: want an error")
	}
	if err := h.a.Rollback(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if got := h.active(t, ""); got != "r1" {
		t.Errorf("single release = %q, want r1", got)
	}
}

func TestNewestByPrefix(t *testing.T) {
	rels := []Release{{Tag: "play-r9"}, {Tag: "r12"}, {Tag: "play-r10"}, {Tag: "admin-r3"}, {Tag: "play-r10x"}, {Tag: "xplay-r11"}, {Tag: "r2"}}
	tests := []struct{ prefix, want string }{
		{"play-", "play-r10"}, // by number, not by order or as text
		{"admin-", "admin-r3"},
		{"", "r12"},
		{"agent-", ""},
	}
	for _, tt := range tests {
		got := ""
		if r := newest(rels, tt.prefix); r != nil {
			got = r.Tag
		}
		if got != tt.want {
			t.Errorf("newest(%q) = %q, want %q", tt.prefix, got, tt.want)
		}
	}
}

func TestOneListCallPerRunWithETag(t *testing.T) {
	h := migrated(t)
	h.github.mu.Lock()
	h.github.lists, h.github.notMod = 0, 0
	h.github.mu.Unlock()

	for range 2 {
		if err := h.a.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	h.github.mu.Lock()
	lists, notMod := h.github.lists, h.github.notMod
	h.github.mu.Unlock()
	if lists != 2 || notMod != 2 {
		t.Errorf("%d list calls, %d answered 304; want 2 and 2 (one per run, both from the ETag)", lists, notMod)
	}

	// A new release changes the ETag, and the cached list isn't used.
	h.pub(t, "play-r2", "play", "/ok")
	if err := h.a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.active(t, "play"); got != "play-r2" {
		t.Errorf("play = %q, want play-r2", got)
	}
}

// The old agent only knows the single release; track releases published
// next to it mustn't change what it picks.
func TestSingleIgnoresTrackReleases(t *testing.T) {
	h := newAgentHarness(t)
	h.pub(t, "r1", "", "/ok")
	h.pub(t, "play-r5", "play", "/ok")
	rel, err := h.d.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel.Tag != "r1" {
		t.Errorf("latest single release = %q, want r1", rel.Tag)
	}
}

func TestInstallRejectsOtherTrack(t *testing.T) {
	h := newAgentHarness(t)
	// Published as play's, built as admin's.
	h.github.publish("play-r1", tarball(t, trackFiles(t, "admin", h.health.URL+"/ok")))
	d, _ := h.a.TrackDeployer("play")
	rel, err := d.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Install(context.Background(), rel); err == nil || !strings.Contains(err.Error(), `track "admin"`) {
		t.Errorf("err = %v, want the wrong track refused", err)
	}
}

func TestSelfCheckPicksLayoutByVersion(t *testing.T) {
	h := migrated(t)
	if err := h.a.SelfCheck(context.Background(), "agent-r1"); err != nil {
		t.Errorf("agent-r1: %v", err)
	}
	if err := h.a.SelfCheck(context.Background(), "agent-r2"); err == nil {
		t.Error("agent-r2 while agent-r1 is active: want an error")
	}
	h2 := newAgentHarness(t)
	h2.pub(t, "r1", "", "/ok")
	h2.d.Run(context.Background())
	if err := h2.a.SelfCheck(context.Background(), "r1"); err != nil {
		t.Errorf("r1 on the single layout: %v", err)
	}
}

func TestNewerStateRefused(t *testing.T) {
	h := newAgentHarness(t)
	os.WriteFile(filepath.Join(h.a.Apps, state.FileName), []byte(`{"version": 99}`), 0o644)
	if err := h.a.Run(context.Background()); !errors.Is(err, state.ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
	if _, err := h.a.Status(); !errors.Is(err, state.ErrUnsupported) {
		t.Errorf("status err = %v, want ErrUnsupported", err)
	}
}
