package deploy

import (
	"bytes"
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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeProvenance accepts a bundle that reads "attests <hex digest>", like
// one from the release workflow, and rejects anything else, like one
// signed by another repo or workflow. The real check is tested with
// recorded bundles in internal/provenance.
type fakeProvenance struct {
	mu      sync.Mutex
	digests []string // digests Verify was asked about
}

func (p *fakeProvenance) Verify(bundle []byte, digest []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	d := hex.EncodeToString(digest)
	p.digests = append(p.digests, d)
	if string(bundle) != "attests "+d {
		return errors.New("provenance: signed by the wrong workflow")
	}
	return nil
}

// attested serves h.github's releases with a provenance asset, as the
// release workflow publishes them, and counts tarball downloads.
type attested struct {
	*httptest.Server
	mu        sync.Mutex
	bundles   map[string]string // tag -> bundle; a missing tag has no provenance asset
	downloads atomic.Int32
}

func withProvenance(t *testing.T, h *harness) (*attested, *fakeProvenance) {
	a := &attested{bundles: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		resp, err := http.Get(h.github.URL + r.URL.Path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			w.WriteHeader(resp.StatusCode)
			return
		}
		var rel map[string]any
		json.NewDecoder(resp.Body).Decode(&rel)
		tag := rel["tag_name"].(string)
		assets := rel["assets"].([]any)
		for _, asset := range assets {
			asset := asset.(map[string]any)
			if asset["name"] == TarballName {
				// Through this server, to count downloads.
				asset["browser_download_url"] = a.URL + "/tarball/" + tag
			}
		}
		a.mu.Lock()
		if _, ok := a.bundles[tag]; ok {
			assets = append(assets, map[string]any{
				"name": ProvenanceName, "browser_download_url": a.URL + "/provenance/" + tag,
			})
		}
		a.mu.Unlock()
		rel["assets"] = assets
		json.NewEncoder(w).Encode(rel)
	})
	mux.HandleFunc("GET /tarball/{tag}", func(w http.ResponseWriter, r *http.Request) {
		a.downloads.Add(1)
		http.Redirect(w, r, fmt.Sprintf("%s/dl/%s/%s", h.github.URL, r.PathValue("tag"), TarballName), http.StatusFound)
	})
	mux.HandleFunc("GET /provenance/{tag}", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		io.WriteString(w, a.bundles[r.PathValue("tag")])
	})
	a.Server = httptest.NewServer(mux)
	t.Cleanup(a.Close)

	p := &fakeProvenance{}
	h.d.API = a.URL
	h.d.Provenance = p
	return a, p
}

// attest publishes a bundle for tag that attests to its tarball.
func (a *attested) attest(h *harness, tag string) {
	h.github.mu.Lock()
	sum := sha256.Sum256(h.github.tarballs[tag])
	h.github.mu.Unlock()
	a.setBundle(tag, "attests "+hex.EncodeToString(sum[:]))
}

func (a *attested) setBundle(tag, bundle string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.bundles[tag] = bundle
}

// assertNotInstalled checks a refused release left nothing behind and the
// active release is unchanged.
func assertNotInstalled(t *testing.T, h *harness, tag, active string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(h.d.Apps, "releases", tag)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s is installed after it was refused (stat: %v)", tag, err)
	}
	for _, pattern := range []string{
		filepath.Join(h.d.Apps, ".download-*"),
		filepath.Join(h.d.Apps, "releases", ".tmp-*"),
	} {
		if m, _ := filepath.Glob(pattern); len(m) > 0 {
			t.Errorf("left behind %q", m)
		}
	}
	if got := h.current(t); got != active {
		t.Errorf("current = %q, want %q", got, active)
	}
}

func TestAttestedReleaseInstalls(t *testing.T) {
	h := newHarness(t)
	a, p := withProvenance(t, h)
	h.publish(t, "r1", "/ok")
	a.attest(h, "r1")

	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.current(t); got != "r1" {
		t.Errorf("current = %q, want r1", got)
	}
	sum := sha256.Sum256(h.github.tarballs["r1"])
	if want := hex.EncodeToString(sum[:]); len(p.digests) != 1 || p.digests[0] != want {
		t.Errorf("verified digests %q, want [%s]", p.digests, want)
	}
}

func TestBadAttestationIsRefused(t *testing.T) {
	h := newHarness(t)
	a, _ := withProvenance(t, h)
	h.publish(t, "r1", "/ok")
	a.attest(h, "r1")
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.takeCalls()

	// Published by someone who can create releases, with a bundle that
	// isn't from the release workflow.
	h.publish(t, "r2", "/ok")
	a.setBundle("r2", "signed by someone else")
	before := a.downloads.Load()
	err := h.d.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refusing to install r2") {
		t.Fatalf("Run = %v, want it to refuse r2", err)
	}
	if a.downloads.Load() != before {
		t.Error("the tarball was downloaded before its provenance was checked")
	}
	assertNotInstalled(t, h, "r2", "r1")
	if calls := h.takeCalls(); len(calls) > 0 {
		t.Errorf("systemctl was called for a refused release: %q", calls)
	}
}

func TestMissingAttestationIsRefused(t *testing.T) {
	h := newHarness(t)
	a, _ := withProvenance(t, h)
	h.publish(t, "r1", "/ok")

	err := h.d.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "has no "+ProvenanceName) {
		t.Fatalf("Run = %v, want it to refuse a release without provenance", err)
	}
	if a.downloads.Load() != 0 {
		t.Error("the tarball was downloaded")
	}
	assertNotInstalled(t, h, "r1", "")
}

// The checksum file and the bundle agree, but the tarball was swapped: the
// checksum check that follows catches it, so what's unpacked is always
// what was attested.
func TestTarballSwappedAfterAttestationIsRefused(t *testing.T) {
	h := newHarness(t)
	a, _ := withProvenance(t, h)
	h.publish(t, "r1", "/ok")
	a.attest(h, "r1")
	h.github.mu.Lock()
	sum := sha256.Sum256(h.github.tarballs["r1"])
	h.github.checksums["r1"] = hex.EncodeToString(sum[:])
	h.github.tarballs["r1"] = tarball(t, releaseFiles(t, h.health.URL+"/ok/evil"))
	h.github.mu.Unlock()

	err := h.d.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("Run = %v, want a checksum mismatch", err)
	}
	assertNotInstalled(t, h, "r1", "")
}

func TestUnattestedBefore(t *testing.T) {
	tests := []struct {
		cutoff, tag string
		bundle      string // "" publishes no provenance asset
		installs    bool
	}{
		{cutoff: "r3", tag: "r2", installs: true},
		{cutoff: "r10", tag: "r9", installs: true}, // compared as numbers
		{cutoff: "r3", tag: "r3"},
		{cutoff: "r3", tag: "r4"},
		{cutoff: "r3", tag: "v1"},
		{cutoff: "", tag: "r1"},
		{cutoff: "bad", tag: "r1"},
		// A release that has a bundle is always checked.
		{cutoff: "r3", tag: "r2", bundle: "signed by someone else"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s_%s_%t", tt.cutoff, tt.tag, tt.bundle != ""), func(t *testing.T) {
			h := newHarness(t)
			a, _ := withProvenance(t, h)
			var logs bytes.Buffer
			h.d.Log = log.New(&logs, "", 0)
			h.d.UnattestedBefore = tt.cutoff
			h.publish(t, tt.tag, "/ok")
			if tt.bundle != "" {
				a.setBundle(tt.tag, tt.bundle)
			}

			err := h.d.Run(context.Background())
			if tt.installs {
				if err != nil {
					t.Fatal(err)
				}
				if got := h.current(t); got != tt.tag {
					t.Errorf("current = %q, want %s", got, tt.tag)
				}
				if !strings.Contains(logs.String(), "WARNING: installing "+tt.tag+" without checking its provenance") {
					t.Errorf("no warning logged:\n%s", logs.String())
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "refusing to install") {
				t.Fatalf("Run = %v, want it refused", err)
			}
			assertNotInstalled(t, h, tt.tag, "")
		})
	}
}

func TestValidCutoff(t *testing.T) {
	for tag, want := range map[string]bool{"r1": true, "r123": true, "": false, "r": false, "v1": false, "r1a": false, "r-1": false} {
		if got := ValidCutoff(tag); got != want {
			t.Errorf("ValidCutoff(%q) = %t, want %t", tag, got, want)
		}
	}
}
