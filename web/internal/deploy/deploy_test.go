package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// entry is one file in a test tarball.
type entry struct {
	name string
	body string
	mode int64
	kind byte // tar type; 0 means a regular file
}

func tarball(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.body)), Typeflag: e.kind}
		if hdr.Typeflag == 0 {
			hdr.Typeflag = tar.TypeReg
		}
		if hdr.Mode == 0 {
			hdr.Mode = 0o644
		}
		if hdr.Typeflag == tar.TypeSymlink {
			hdr.Linkname, hdr.Size = e.body, 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// releaseFiles is a valid release whose health check is healthURL.
func releaseFiles(t *testing.T, healthURL string) []entry {
	manifest, _ := json.Marshal(Manifest{
		Units:    []string{"play.service", "mc-agent.timer"},
		Services: []string{"play.service"},
		Timers:   []string{"mc-agent.timer"},
		Health:   []string{healthURL},
	})
	return []entry{
		{name: "bin/play", body: "#!/bin/sh\n", mode: 0o755},
		{name: "deploy/systemd/play.service", body: "[Service]\n"},
		{name: "deploy/systemd/mc-agent.timer", body: "[Timer]\n"},
		{name: ManifestPath, body: string(manifest)},
	}
}

// fakeGitHub serves releases and their assets.
type fakeGitHub struct {
	*httptest.Server
	mu        sync.Mutex
	latest    string
	tarballs  map[string][]byte
	checksums map[string]string // overrides the real checksum
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	g := &fakeGitHub{tarballs: map[string][]byte{}, checksums: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.latest == "" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"tag_name": %q, "assets": [
			{"name": %q, "browser_download_url": "%s/dl/%s/%s"},
			{"name": %q, "browser_download_url": "%s/dl/%s/%s"}]}`,
			g.latest,
			TarballName, g.URL, g.latest, TarballName,
			ChecksumName, g.URL, g.latest, ChecksumName)
	})
	mux.HandleFunc("GET /dl/{tag}/{name}", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		tag := r.PathValue("tag")
		data := g.tarballs[tag]
		switch r.PathValue("name") {
		case TarballName:
			w.Write(data)
		case ChecksumName:
			sum := sha256.Sum256(data)
			hexSum := hex.EncodeToString(sum[:])
			if s, ok := g.checksums[tag]; ok {
				hexSum = s
			}
			fmt.Fprintf(w, "%s  %s\n", hexSum, TarballName)
		}
	})
	g.Server = httptest.NewServer(mux)
	t.Cleanup(g.Close)
	return g
}

func (g *fakeGitHub) publish(tag string, data []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tarballs[tag] = data
	g.latest = tag
}

// health serves /ok (200) and /bad (500).
func newHealth(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ok" {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

type harness struct {
	d      *Deployer
	github *fakeGitHub
	health *httptest.Server

	mu    sync.Mutex
	calls []string
	// failWhen makes systemctl fail when it returns true.
	failWhen func(current string, args []string) bool
}

func newHarness(t *testing.T) *harness {
	h := &harness{github: newFakeGitHub(t), health: newHealth(t)}
	root := t.TempDir()
	h.d = &Deployer{
		Repo:          "o/r",
		API:           h.github.URL,
		Apps:          filepath.Join(root, "apps"),
		UnitDir:       filepath.Join(root, "units"),
		HTTP:          http.DefaultClient,
		HealthTimeout: time.Second,
		Keep:          3,
		Log:           log.New(io.Discard, "", 0),
		Systemctl: func(ctx context.Context, args ...string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			err := h.systemctl(args)
			call := strings.Join(args, " ")
			if err != nil {
				call += " (failed)"
			}
			h.calls = append(h.calls, call)
			return err
		},
	}
	if err := os.MkdirAll(h.d.Apps, 0o755); err != nil {
		t.Fatal(err)
	}
	return h
}

// systemctl is the fake systemctl's behavior; calls are recorded by the
// caller, with failed ones marked.
func (h *harness) systemctl(args []string) error {
	current, _ := h.d.Current()
	if h.failWhen != nil && h.failWhen(current, args) {
		return errors.New("unit failed")
	}
	// Like systemctl, disable needs each unit's file to exist.
	if args[0] == "disable" {
		for _, unit := range args[1:] {
			if strings.HasPrefix(unit, "-") {
				continue
			}
			if _, err := os.Stat(filepath.Join(h.d.UnitDir, unitFile(unit))); err != nil {
				return fmt.Errorf("unit file %s does not exist", unit)
			}
		}
	}
	return nil
}

func (h *harness) publish(t *testing.T, tag, healthPath string) {
	h.github.publish(tag, tarball(t, releaseFiles(t, h.health.URL+healthPath)))
}

func (h *harness) takeCalls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.calls
	h.calls = nil
	return c
}

func (h *harness) current(t *testing.T) string {
	t.Helper()
	tag, err := h.d.Current()
	if err != nil {
		t.Fatal(err)
	}
	return tag
}

// age backdates a release so installs within one test have distinct times.
func (h *harness) age(t *testing.T, tag string, by time.Duration) {
	when := time.Now().Add(-by)
	if err := os.Chtimes(filepath.Join(h.d.Apps, "releases", tag), when, when); err != nil {
		t.Fatal(err)
	}
}

var applyCalls = []string{
	"daemon-reload",
	"enable play.service",
	"restart play.service",
	"enable --now mc-agent.timer",
}

func TestFreshDeploy(t *testing.T) {
	h := newHarness(t)
	h.publish(t, "r1", "/ok")

	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.current(t); got != "r1" {
		t.Errorf("current = %q, want r1", got)
	}
	if got := h.takeCalls(); !reflect.DeepEqual(got, applyCalls) {
		t.Errorf("systemctl calls = %q, want %q", got, applyCalls)
	}

	// Units are linked through current, not to a fixed release.
	link, err := os.Readlink(filepath.Join(h.d.UnitDir, "play.service"))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(h.d.Apps, "current", "deploy", "systemd", "play.service"); link != want {
		t.Errorf("play.service links to %s, want %s", link, want)
	}

	info, err := os.Stat(filepath.Join(h.d.Apps, "current", "bin", "play"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("bin/play mode %v lost its executable bit", info.Mode())
	}
}

func TestAlreadyActive(t *testing.T) {
	h := newHarness(t)
	h.publish(t, "r1", "/ok")
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.takeCalls()

	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls := h.takeCalls(); len(calls) != 0 {
		t.Errorf("an up-to-date run called systemctl: %q", calls)
	}
}

func TestNoReleaseYet(t *testing.T) {
	h := newHarness(t)
	if err := h.d.Run(context.Background()); err == nil {
		t.Error("want an error when the repo has no releases")
	}
}

func TestUpgradeAndPrune(t *testing.T) {
	h := newHarness(t)
	h.d.Keep = 2
	for i, tag := range []string{"r1", "r2", "r3"} {
		h.publish(t, tag, "/ok")
		if err := h.d.Run(context.Background()); err != nil {
			t.Fatalf("%s: %v", tag, err)
		}
		h.age(t, tag, time.Duration(10-i)*time.Minute)
	}

	if got := h.current(t); got != "r3" {
		t.Errorf("current = %q, want r3", got)
	}
	tags, err := h.d.installed()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"r3", "r2"}; !reflect.DeepEqual(tags, want) {
		t.Errorf("installed = %q, want %q (r1 pruned)", tags, want)
	}
}

func TestHealthFailureRollsBack(t *testing.T) {
	h := newHarness(t)
	h.d.HealthTimeout = 300 * time.Millisecond
	h.publish(t, "r1", "/ok")
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.takeCalls()

	h.publish(t, "r2", "/bad")
	err := h.d.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "rolled back to r1") {
		t.Fatalf("err = %v, want a rollback to r1", err)
	}
	if got := h.current(t); got != "r1" {
		t.Errorf("current = %q, want r1 after rollback", got)
	}
	// r2 was applied, then r1 was applied again.
	if got, want := h.takeCalls(), append(append([]string{}, applyCalls...), applyCalls...); !reflect.DeepEqual(got, want) {
		t.Errorf("systemctl calls = %q, want %q", got, want)
	}

	// The broken release isn't retried every timer tick.
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls := h.takeCalls(); len(calls) != 0 {
		t.Errorf("a known-bad release was retried: %q", calls)
	}

	// A fixed release clears the failure.
	h.publish(t, "r3", "/ok")
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.current(t); got != "r3" {
		t.Errorf("current = %q, want r3", got)
	}
	if _, err := os.Stat(filepath.Join(h.d.Apps, "failed")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the failed marker was not cleared")
	}
}

func TestSystemctlFailureRollsBack(t *testing.T) {
	h := newHarness(t)
	h.publish(t, "r1", "/ok")
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	h.failWhen = func(current string, args []string) bool { return current == "r2" && args[0] == "restart" }
	h.publish(t, "r2", "/ok")
	if err := h.d.Run(context.Background()); err == nil {
		t.Fatal("want an error")
	}
	if got := h.current(t); got != "r1" {
		t.Errorf("current = %q, want r1 after rollback", got)
	}
}

func TestFirstDeployFailure(t *testing.T) {
	h := newHarness(t)
	h.d.HealthTimeout = 300 * time.Millisecond
	h.publish(t, "r1", "/bad")

	err := h.d.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "nothing to roll back to") {
		t.Errorf("err = %v, want a failure with nothing to roll back to", err)
	}
}

func TestChecksumMismatch(t *testing.T) {
	h := newHarness(t)
	h.publish(t, "r1", "/ok")
	h.github.checksums["r1"] = strings.Repeat("0", 64)

	err := h.d.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v, want a checksum mismatch", err)
	}
	if _, err := os.Stat(filepath.Join(h.d.Apps, "releases", "r1")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a release with a bad checksum was installed")
	}
	if got := h.current(t); got != "" {
		t.Errorf("current = %q, want none", got)
	}
}

func TestMalformedChecksum(t *testing.T) {
	h := newHarness(t)
	h.publish(t, "r1", "/ok")
	h.github.checksums["r1"] = "not-hex"
	if err := h.d.Run(context.Background()); err == nil {
		t.Error("want an error")
	}
}

func TestInvalidTag(t *testing.T) {
	h := newHarness(t)
	h.publish(t, "../escape", "/ok")
	if _, err := h.d.Latest(context.Background()); err == nil {
		t.Error("want an error for a tag that isn't a plain name")
	}
}

func TestRejectsUnsafeReleases(t *testing.T) {
	valid := func(t *testing.T) []entry { return releaseFiles(t, "http://unused/ok") }
	tests := map[string]func(t *testing.T) []entry{
		"parent path":   func(t *testing.T) []entry { return append(valid(t), entry{name: "../evil", body: "x"}) },
		"absolute path": func(t *testing.T) []entry { return append(valid(t), entry{name: "/tmp/evil", body: "x"}) },
		"symlink": func(t *testing.T) []entry {
			return append(valid(t), entry{name: "bin/link", body: "/etc/passwd", kind: tar.TypeSymlink})
		},
		"no manifest": func(t *testing.T) []entry { return valid(t)[:3] },
		"unit outside release": func(t *testing.T) []entry {
			return []entry{{name: ManifestPath, body: `{"units": ["missing.service"]}`}}
		},
		"unit name with slash": func(t *testing.T) []entry {
			return []entry{{name: ManifestPath, body: `{"services": ["../../x.service"]}`}}
		},
		"unit name like a flag": func(t *testing.T) []entry {
			return []entry{{name: ManifestPath, body: `{"services": ["--global"]}`}}
		},
	}
	for name, files := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.github.publish("r1", tarball(t, files(t)))
			if err := h.d.Run(context.Background()); err == nil {
				t.Fatal("want an error")
			}
			if entries, _ := os.ReadDir(filepath.Join(h.d.Apps, "releases")); len(entries) != 0 {
				t.Errorf("releases/ holds %d entries after a rejected release", len(entries))
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(h.d.Apps), "evil")); err == nil {
				t.Error("a file was written outside the release folder")
			}
		})
	}
}

func TestRollbackCommand(t *testing.T) {
	h := newHarness(t)
	for i, tag := range []string{"r1", "r2"} {
		h.publish(t, tag, "/ok")
		if err := h.d.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		h.age(t, tag, time.Duration(10-i)*time.Minute)
	}

	if err := h.d.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.current(t); got != "r1" {
		t.Errorf("current = %q, want r1", got)
	}
}

func TestRollbackWithNothingElse(t *testing.T) {
	h := newHarness(t)
	h.publish(t, "r1", "/ok")
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Rollback(context.Background()); err == nil {
		t.Error("want an error with only one release installed")
	}
}

// publishWith publishes a release with manifest m, healthy unless
// healthPath says otherwise, shipping a unit file for each of m.Units.
func (h *harness) publishWith(t *testing.T, tag, healthPath string, m Manifest) {
	m.Health = []string{h.health.URL + healthPath}
	manifest, _ := json.Marshal(m)
	files := []entry{{name: ManifestPath, body: string(manifest)}}
	for _, unit := range m.Units {
		files = append(files, entry{name: "deploy/systemd/" + unit, body: "[Unit]\n"})
	}
	h.github.publish(tag, tarball(t, files))
}

func (h *harness) linked(unit string) bool {
	_, err := os.Lstat(filepath.Join(h.d.UnitDir, unit))
	return err == nil
}

// before reports whether call a comes before call b in calls.
func before(calls []string, a, b string) bool {
	i, j := slices.Index(calls, a), slices.Index(calls, b)
	return i >= 0 && j >= 0 && i < j
}

// disabledAt returns the index of the first successful "disable --now"
// call in calls that names unit, or -1.
func disabledAt(calls []string, unit string) int {
	return slices.IndexFunc(calls, func(c string) bool {
		if strings.HasSuffix(c, " (failed)") {
			return false
		}
		f := strings.Fields(c)
		return len(f) > 2 && f[0] == "disable" && f[1] == "--now" && slices.Contains(f[2:], unit)
	})
}

var withOldAndAdmin = Manifest{
	Units:    []string{"play.service", "old.service", "site@.service", "mc-agent.timer"},
	Services: []string{"play.service", "old.service", "site@admin.service"},
	Timers:   []string{"mc-agent.timer"},
}

var withoutThem = Manifest{
	Units:    []string{"play.service", "site@.service", "mc-agent.timer"},
	Services: []string{"play.service", "site@portfolio.service"},
	Timers:   []string{"mc-agent.timer"},
}

func TestUpgradeRemovesDroppedUnits(t *testing.T) {
	h := newHarness(t)
	h.publishWith(t, "r1", "/ok", withOldAndAdmin)
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.takeCalls()

	h.publishWith(t, "r2", "/ok", withoutThem)
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := h.takeCalls()
	// site@admin.service is an instance: its template stays, but it goes.
	restart := slices.Index(calls, "restart play.service site@portfolio.service")
	for _, unit := range []string{"old.service", "site@admin.service"} {
		if i := disabledAt(calls, unit); i < 0 || i > restart {
			t.Errorf("%s wasn't stopped and disabled before the restart: %q", unit, calls)
		}
	}
	if disabledAt(calls, "play.service") >= 0 {
		t.Errorf("a kept service was stopped: %q", calls)
	}
	// It all went in one call, which only works while the files exist.
	if calls[0] != "disable --now old.service site@admin.service" {
		t.Errorf("first call = %q, want both disabled together", calls[0])
	}
	if h.linked("old.service") {
		t.Error("old.service is still linked")
	}
	if !h.linked("site@.service") {
		t.Error("the site@.service template was unlinked, but r2 still ships it")
	}

	// Once removed, they aren't removed again on the next deploy.
	h.publishWith(t, "r3", "/ok", withoutThem)
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range h.takeCalls() {
		if strings.HasPrefix(c, "disable") {
			t.Errorf("r3 disabled something again: %q", c)
		}
	}
}

func TestRollbackRemovesTheFailedReleasesUnits(t *testing.T) {
	h := newHarness(t)
	h.d.HealthTimeout = 300 * time.Millisecond
	h.publishWith(t, "r1", "/ok", withoutThem)
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.takeCalls()

	// r2 adds new.service and fails its health check.
	r2 := withoutThem
	r2.Units = append(slices.Clone(r2.Units), "new.service")
	r2.Services = append(slices.Clone(r2.Services), "new.service")
	h.publishWith(t, "r2", "/bad", r2)
	if err := h.d.Run(context.Background()); err == nil {
		t.Fatal("want r2 to fail")
	}
	if got := h.current(t); got != "r1" {
		t.Fatalf("current = %q, want r1", got)
	}
	calls := h.takeCalls()
	// While r2's unit file still exists, so the real disable succeeds.
	if !slices.Contains(calls, "disable --now new.service") {
		t.Errorf("new.service wasn't stopped and disabled on rollback: %q", calls)
	}
	if h.linked("new.service") {
		t.Error("new.service is still linked after the rollback")
	}
}

func TestInterruptedDeployIsCleanedUp(t *testing.T) {
	h := newHarness(t)
	h.publishWith(t, "r1", "/ok", withoutThem)
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.takeCalls()

	// An agent that died partway through a deploy left ghost.service
	// recorded, linked and enabled.
	ghost := filepath.Join(h.d.Apps, "current", "deploy", "systemd", "ghost.service")
	if err := os.WriteFile(ghost, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ghost, filepath.Join(h.d.UnitDir, "ghost.service")); err != nil {
		t.Fatal(err)
	}
	if err := h.d.writeManaged(union(&withoutThem, &Manifest{Units: []string{"ghost.service"}, Services: []string{"ghost.service"}})); err != nil {
		t.Fatal(err)
	}

	h.publishWith(t, "r2", "/ok", withoutThem)
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls := h.takeCalls(); disabledAt(calls, "ghost.service") < 0 {
		t.Errorf("ghost.service wasn't stopped: %q", calls)
	}
	if h.linked("ghost.service") {
		t.Error("ghost.service is still linked")
	}
}

func TestFirstRunWithoutManagedFile(t *testing.T) {
	h := newHarness(t)
	h.publishWith(t, "r1", "/ok", withOldAndAdmin)
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.takeCalls()
	// r1 was installed by an agent that didn't record what it managed.
	if err := os.Remove(h.d.managedFile()); err != nil {
		t.Fatal(err)
	}

	h.publishWith(t, "r2", "/ok", withoutThem)
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := h.takeCalls()
	for _, unit := range []string{"old.service", "site@admin.service"} {
		if disabledAt(calls, unit) < 0 {
			t.Errorf("%s wasn't removed using r1's manifest: %q", unit, calls)
		}
	}
}

func TestKeepsLinksOutsideApps(t *testing.T) {
	h := newHarness(t)
	h.publishWith(t, "r1", "/ok", withoutThem)
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A hand-installed unit, linked from the server's checkout, that
	// managed.json somehow lists.
	if err := os.Symlink("/home/pi/minecraft_server/deploy/systemd/minecraft.service", filepath.Join(h.d.UnitDir, "minecraft.service")); err != nil {
		t.Fatal(err)
	}
	if err := h.d.writeManaged(union(&withoutThem, &Manifest{Units: []string{"minecraft.service"}, Services: []string{"minecraft.service"}})); err != nil {
		t.Fatal(err)
	}
	h.takeCalls()

	h.publishWith(t, "r2", "/ok", withoutThem)
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !h.linked("minecraft.service") {
		t.Error("a link outside apps was removed")
	}
	if calls := h.takeCalls(); disabledAt(calls, "minecraft.service") >= 0 {
		t.Errorf("a hand-installed service was stopped: %q", calls)
	}
}

func TestRefusesToReplaceHandInstalledUnits(t *testing.T) {
	h := newHarness(t)
	h.d.HealthTimeout = 300 * time.Millisecond
	if err := os.MkdirAll(h.d.UnitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hand := "/home/pi/minecraft_server/deploy/systemd/play.service"
	if err := os.Symlink(hand, filepath.Join(h.d.UnitDir, "play.service")); err != nil {
		t.Fatal(err)
	}

	h.publishWith(t, "r1", "/ok", withoutThem)
	err := h.d.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "installed by hand") {
		t.Fatalf("err = %v, want a refusal to replace the hand-installed link", err)
	}
	if got, _ := os.Readlink(filepath.Join(h.d.UnitDir, "play.service")); got != hand {
		t.Errorf("play.service now links to %q, want it left at %q", got, hand)
	}
}

func TestUnreadableManagedFileDoesntBlockDeploys(t *testing.T) {
	h := newHarness(t)
	h.publishWith(t, "r1", "/ok", withOldAndAdmin)
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.takeCalls()
	// A power cut left it truncated.
	if err := os.WriteFile(h.d.managedFile(), []byte(`{"units": ["pla`), 0o644); err != nil {
		t.Fatal(err)
	}

	h.publishWith(t, "r2", "/ok", withoutThem)
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatalf("a corrupt managed.json blocked the deploy: %v", err)
	}
	// It fell back to r1's manifest.
	if calls := h.takeCalls(); disabledAt(calls, "old.service") < 0 {
		t.Errorf("old.service wasn't removed: %q", calls)
	}
}

func TestUnlinksUnitsShadowingTheRelease(t *testing.T) {
	h := newHarness(t)
	if err := os.MkdirAll(h.d.UnitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Installed by hand before mc-agent existed: the instance links to the
	// template in the git checkout, which systemd prefers over the
	// release's site@.service.
	checkout := "/home/pi/minecraft_server/deploy/systemd/site@.service"
	if err := os.Symlink(checkout, filepath.Join(h.d.UnitDir, "site@portfolio.service")); err != nil {
		t.Fatal(err)
	}

	h.publishWith(t, "r1", "/ok", withoutThem)
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.linked("site@portfolio.service") {
		t.Error("site@portfolio.service still shadows the release's template")
	}
	// The usual enable after daemon-reload moves its enable links; nothing
	// extra is needed, so an interrupted fix-up is finished by any later run.
	if got := h.takeCalls(); !reflect.DeepEqual(got, []string{
		"daemon-reload",
		"enable play.service site@portfolio.service",
		"restart play.service site@portfolio.service",
		"enable --now mc-agent.timer",
	}) {
		t.Errorf("systemctl calls = %q", got)
	}
}

func TestKeepsLinksThatAreNotShadowing(t *testing.T) {
	h := newHarness(t)
	h.publishWith(t, "r1", "/ok", withoutThem)
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A relative link to the release's own template, through current.
	rel, err := filepath.Rel(h.d.UnitDir, filepath.Join(h.d.Apps, "current", "deploy", "systemd", "site@.service"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rel, filepath.Join(h.d.UnitDir, "site@portfolio.service")); err != nil {
		t.Fatal(err)
	}
	// A hand-installed unit that's no template instance of the release.
	m := withoutThem
	m.Services = append(slices.Clone(m.Services), "minecraft.service")
	if err := os.Symlink("/home/pi/minecraft_server/deploy/systemd/minecraft.service", filepath.Join(h.d.UnitDir, "minecraft.service")); err != nil {
		t.Fatal(err)
	}

	h.publishWith(t, "r2", "/ok", m)
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !h.linked("site@portfolio.service") {
		t.Error("a relative link into the release was removed")
	}
	if !h.linked("minecraft.service") {
		t.Error("a hand-installed unit the release doesn't ship was removed")
	}
}

func TestShadowingFileFailsTheDeploy(t *testing.T) {
	h := newHarness(t)
	if err := os.MkdirAll(h.d.UnitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(h.d.UnitDir, "site@portfolio.service")
	if err := os.WriteFile(path, []byte("[Service]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	h.publishWith(t, "r1", "/ok", withoutThem)
	err := h.d.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "overrides the release") {
		t.Fatalf("err = %v, want the shadowing file named", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the hand-written file was removed: %v", err)
	}
}

func (h *harness) applied(t *testing.T) *Applied {
	t.Helper()
	a, err := h.d.LastApplied()
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// appliedBetween fails unless the active release is tag, recorded as
// applied between from and to.
func (h *harness) appliedBetween(t *testing.T, tag string, from, to time.Time) {
	t.Helper()
	a := h.applied(t)
	if a == nil {
		t.Fatalf("no applied time recorded, want %s", tag)
	}
	if a.Tag != tag || a.At.Before(from) || a.At.After(to) {
		t.Errorf("applied = %s at %v, want %s between %v and %v", a.Tag, a.At, tag, from, to)
	}
}

func TestRecordsWhenAReleaseIsApplied(t *testing.T) {
	h := newHarness(t)
	if a := h.applied(t); a != nil {
		t.Errorf("applied = %+v before any deploy, want nil", a)
	}

	h.publish(t, "r1", "/ok")
	before := time.Now()
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.appliedBetween(t, "r1", before, time.Now())

	// An up-to-date run doesn't switch, so the time stays.
	first := h.applied(t).At
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.applied(t).At; !got.Equal(first) {
		t.Errorf("applied at %v after an up-to-date run, want %v", got, first)
	}
}

func TestRecordsWhenARollbackIsApplied(t *testing.T) {
	h := newHarness(t)
	h.d.HealthTimeout = 300 * time.Millisecond
	h.publish(t, "r1", "/ok")
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	// r2 fails its health check; r1 is applied again, so it gets a new time.
	h.publish(t, "r2", "/bad")
	before := time.Now()
	if err := h.d.Run(context.Background()); err == nil {
		t.Fatal("want r2 to fail")
	}
	h.appliedBetween(t, "r1", before, time.Now())

	// The rollback command too.
	h.publish(t, "r3", "/ok")
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Rollback picks the newest other release; make that r1, not r2.
	h.age(t, "r3", time.Minute)
	h.age(t, "r1", 10*time.Minute)
	h.age(t, "r2", 20*time.Minute)
	before = time.Now()
	if err := h.d.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.appliedBetween(t, "r1", before, time.Now())
}

func TestAppliedTimeOfAnotherReleaseIsIgnored(t *testing.T) {
	h := newHarness(t)
	h.publish(t, "r1", "/ok")
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	// As if an agent from before active.json switched to r1 after r0.
	data, _ := json.Marshal(Applied{Tag: "r0", At: time.Now()})
	if err := os.WriteFile(filepath.Join(h.d.Apps, "active.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if a := h.applied(t); a != nil {
		t.Errorf("applied = %+v, want nil when it names another release", a)
	}
}

func TestUnwritableAppliedTimeDoesntFailTheDeploy(t *testing.T) {
	h := newHarness(t)
	// A folder where active.json goes makes writing it fail.
	if err := os.Mkdir(filepath.Join(h.d.Apps, "active.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.publish(t, "r1", "/ok")
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.current(t); got != "r1" {
		t.Errorf("current = %q, want r1", got)
	}
}
