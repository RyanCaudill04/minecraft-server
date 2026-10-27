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
			h.calls = append(h.calls, strings.Join(args, " "))
			current, _ := h.d.Current()
			if h.failWhen != nil && h.failWhen(current, args) {
				return errors.New("unit failed")
			}
			return nil
		},
	}
	if err := os.MkdirAll(h.d.Apps, 0o755); err != nil {
		t.Fatal(err)
	}
	return h
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
