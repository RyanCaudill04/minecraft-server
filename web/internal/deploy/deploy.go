// Package deploy installs releases of this repo's web apps on the Pi.
//
// A release is a GitHub Release with two assets: release.tar.gz (binaries,
// sites and unit files) and release.tar.gz.sha256. Releases are unpacked
// into <apps>/releases/<tag>, and <apps>/current is a symlink to the active
// one. Unit files are linked into the systemd user folder through current,
// so switching the link switches every unit at once.
//
// A deploy switches current, links and reloads the units, restarts the
// services listed in the release's deploy/release.json, and waits for its
// health URLs. If any step fails, it switches back to the previous release
// and records the tag as failed, so a broken release isn't retried.
package deploy

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Asset names in each GitHub Release.
const (
	TarballName  = "release.tar.gz"
	ChecksumName = TarballName + ".sha256"
)

// ManifestPath is where the manifest lives inside a release.
const ManifestPath = "deploy/release.json"

// Limits on what a release may contain.
const (
	maxReleaseBytes = 200 << 20
	maxChecksumSize = 1 << 10
)

var validTag = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Manifest says how to activate a release. It is deploy/release.json.
type Manifest struct {
	Units    []string `json:"units"`    // files in deploy/systemd to link into the user unit folder
	Services []string `json:"services"` // enabled and restarted on every deploy
	Timers   []string `json:"timers"`   // enabled and started
	Health   []string `json:"health"`   // URLs that must answer 200 after a deploy
}

// Release is one GitHub Release.
type Release struct {
	Tag    string `json:"tag_name"`
	Assets []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func (r *Release) assetURL(name string) (string, error) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.URL, nil
		}
	}
	return "", fmt.Errorf("release %s has no %s asset", r.Tag, name)
}

// Deployer installs and activates releases.
type Deployer struct {
	Repo    string // GitHub "owner/name"
	API     string // GitHub API base URL, e.g. https://api.github.com
	Apps    string // folder holding releases/ and the current link
	UnitDir string // systemd user unit folder, e.g. ~/.config/systemd/user

	HTTP *http.Client
	// Systemctl runs "systemctl --user" with args; replaced in tests.
	Systemctl func(ctx context.Context, args ...string) error

	HealthTimeout time.Duration // how long each health URL may take to answer 200
	Keep          int           // installed releases to keep, at least 2
	Log           *log.Logger
}

func (d *Deployer) releasesDir() string { return filepath.Join(d.Apps, "releases") }
func (d *Deployer) currentLink() string { return filepath.Join(d.Apps, "current") }
func (d *Deployer) failedFile() string  { return filepath.Join(d.Apps, "failed") }

// Current returns the tag of the active release, or "" if none is active.
func (d *Deployer) Current() (string, error) {
	target, err := os.Readlink(d.currentLink())
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return filepath.Base(target), nil
}

// Latest asks GitHub for the newest published release.
func (d *Deployer) Latest(ctx context.Context) (*Release, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", d.API, d.Repo)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("deploy: fetching latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("deploy: fetching latest release: %s", resp.Status)
	}
	var rel Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("deploy: decoding latest release: %w", err)
	}
	if !validTag.MatchString(rel.Tag) {
		return nil, fmt.Errorf("deploy: invalid release tag %q", rel.Tag)
	}
	return &rel, nil
}

// Run deploys the latest release unless it is already active or has
// failed before. It is what the systemd timer calls.
func (d *Deployer) Run(ctx context.Context) error {
	rel, err := d.Latest(ctx)
	if err != nil {
		return err
	}
	current, err := d.Current()
	if err != nil {
		return err
	}
	if rel.Tag == current {
		d.Log.Printf("%s is already active", current)
		return nil
	}
	if failed, _ := os.ReadFile(d.failedFile()); strings.TrimSpace(string(failed)) == rel.Tag {
		d.Log.Printf("skipping %s: it failed before; publish a new release to retry", rel.Tag)
		return nil
	}

	if err := d.Install(ctx, rel); err != nil {
		return err
	}
	return d.Activate(ctx, rel.Tag)
}

// Install downloads, verifies and unpacks a release into releases/<tag>.
// It does nothing if that release is already installed.
func (d *Deployer) Install(ctx context.Context, rel *Release) error {
	dest := filepath.Join(d.releasesDir(), rel.Tag)
	if _, err := os.Stat(dest); err == nil {
		return nil
	}
	if err := os.MkdirAll(d.releasesDir(), 0o755); err != nil {
		return err
	}
	d.Log.Printf("installing %s", rel.Tag)

	sumURL, err := rel.assetURL(ChecksumName)
	if err != nil {
		return err
	}
	tarURL, err := rel.assetURL(TarballName)
	if err != nil {
		return err
	}
	want, err := d.fetchChecksum(ctx, sumURL)
	if err != nil {
		return err
	}

	// Download to a temporary file, hashing as it goes, and only unpack
	// once the checksum matches.
	tmp, err := os.CreateTemp(d.Apps, ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	hash := sha256.New()
	if err := d.download(ctx, tarURL, io.MultiWriter(tmp, hash), maxReleaseBytes); err != nil {
		return err
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != want {
		return fmt.Errorf("deploy: %s checksum mismatch: got %s, want %s", rel.Tag, got, want)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}

	staging := filepath.Join(d.releasesDir(), ".tmp-"+rel.Tag)
	os.RemoveAll(staging)
	if err := extract(tmp, staging); err != nil {
		os.RemoveAll(staging)
		return fmt.Errorf("deploy: unpacking %s: %w", rel.Tag, err)
	}
	if _, err := readManifest(staging); err != nil {
		os.RemoveAll(staging)
		return fmt.Errorf("deploy: %s: %w", rel.Tag, err)
	}
	return os.Rename(staging, dest)
}

func (d *Deployer) fetchChecksum(ctx context.Context, url string) (string, error) {
	var b strings.Builder
	if err := d.download(ctx, url, &b, maxChecksumSize); err != nil {
		return "", err
	}
	// sha256sum output: "<hex>  <file name>"
	fields := strings.Fields(b.String())
	if len(fields) == 0 || len(fields[0]) != sha256.Size*2 {
		return "", fmt.Errorf("deploy: malformed checksum file")
	}
	if _, err := hex.DecodeString(fields[0]); err != nil {
		return "", fmt.Errorf("deploy: malformed checksum file: %w", err)
	}
	return strings.ToLower(fields[0]), nil
}

func (d *Deployer) download(ctx context.Context, url string, w io.Writer, limit int64) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("deploy: downloading %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("deploy: downloading %s: %s", url, resp.Status)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fmt.Errorf("deploy: downloading %s: %w", url, err)
	}
	if n > limit {
		return fmt.Errorf("deploy: %s is larger than %d bytes", url, limit)
	}
	return nil
}

// Activate makes an installed release current. If activation fails, it
// switches back to the previously active release and marks tag as failed.
func (d *Deployer) Activate(ctx context.Context, tag string) error {
	previous, err := d.Current()
	if err != nil {
		return err
	}
	if err := d.switchTo(tag); err != nil {
		return err
	}
	d.Log.Printf("activating %s (was %q)", tag, previous)

	if err := d.apply(ctx); err != nil {
		d.Log.Printf("%s failed: %v", tag, err)
		os.WriteFile(d.failedFile(), []byte(tag+"\n"), 0o644)
		if previous == "" {
			return fmt.Errorf("deploy: %s failed with nothing to roll back to: %w", tag, err)
		}
		d.Log.Printf("rolling back to %s", previous)
		if rbErr := d.switchTo(previous); rbErr != nil {
			return errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
		}
		if rbErr := d.apply(ctx); rbErr != nil {
			return errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
		}
		return fmt.Errorf("deploy: %s failed and was rolled back to %s: %w", tag, previous, err)
	}

	os.Remove(d.failedFile())
	d.Log.Printf("%s is active", tag)
	return d.prune()
}

// Rollback activates the newest installed release other than the current
// one.
func (d *Deployer) Rollback(ctx context.Context) error {
	current, err := d.Current()
	if err != nil {
		return err
	}
	tags, err := d.installed()
	if err != nil {
		return err
	}
	for _, tag := range tags {
		if tag != current {
			return d.Activate(ctx, tag)
		}
	}
	return errors.New("deploy: no other release is installed")
}

// switchTo points current at releases/<tag>, replacing the link atomically.
func (d *Deployer) switchTo(tag string) error {
	if _, err := os.Stat(filepath.Join(d.releasesDir(), tag)); err != nil {
		return fmt.Errorf("deploy: release %s is not installed: %w", tag, err)
	}
	tmp := d.currentLink() + ".tmp"
	os.Remove(tmp)
	if err := os.Symlink(filepath.Join("releases", tag), tmp); err != nil {
		return err
	}
	return os.Rename(tmp, d.currentLink())
}

// apply links the current release's units, then reloads, enables and
// restarts them and waits until every health URL answers.
func (d *Deployer) apply(ctx context.Context) error {
	m, err := readManifest(d.currentLink())
	if err != nil {
		return err
	}

	if err := os.MkdirAll(d.UnitDir, 0o755); err != nil {
		return err
	}
	for _, unit := range m.Units {
		// Link through current, so the next switch updates the unit too.
		target := filepath.Join(d.currentLink(), "deploy", "systemd", unit)
		link := filepath.Join(d.UnitDir, unit)
		os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			return err
		}
	}

	steps := [][]string{{"daemon-reload"}}
	if len(m.Services) > 0 {
		steps = append(steps,
			append([]string{"enable"}, m.Services...),
			append([]string{"restart"}, m.Services...))
	}
	if len(m.Timers) > 0 {
		steps = append(steps, append([]string{"enable", "--now"}, m.Timers...))
	}
	for _, args := range steps {
		if err := d.Systemctl(ctx, args...); err != nil {
			return fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
		}
	}

	for _, url := range m.Health {
		if err := d.waitHealthy(ctx, url); err != nil {
			return err
		}
	}
	return nil
}

func (d *Deployer) waitHealthy(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, d.HealthTimeout)
	defer cancel()
	var last error
	for {
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return err
		}
		resp, err := d.HTTP.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			err = errors.New(resp.Status)
		}
		last = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("health check %s: %w", url, last)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// installed lists installed release tags, newest first.
func (d *Deployer) installed() ([]string, error) {
	entries, err := os.ReadDir(d.releasesDir())
	if err != nil {
		return nil, err
	}
	type rel struct {
		tag string
		mod time.Time
	}
	var rels []rel
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		rels = append(rels, rel{e.Name(), info.ModTime()})
	}
	sort.Slice(rels, func(i, j int) bool { return rels[i].mod.After(rels[j].mod) })
	tags := make([]string, len(rels))
	for i, r := range rels {
		tags[i] = r.tag
	}
	return tags, nil
}

// prune removes the oldest installed releases beyond Keep, never the
// current one.
func (d *Deployer) prune() error {
	current, err := d.Current()
	if err != nil {
		return err
	}
	tags, err := d.installed()
	if err != nil {
		return err
	}
	kept := 1 // the current release
	for _, tag := range tags {
		if tag == current {
			continue
		}
		if kept < d.Keep {
			kept++
			continue
		}
		d.Log.Printf("removing old release %s", tag)
		if err := os.RemoveAll(filepath.Join(d.releasesDir(), tag)); err != nil {
			return err
		}
	}
	return nil
}

func readManifest(dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, ManifestPath))
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decoding manifest: %w", err)
	}
	for _, name := range append(append(append([]string{}, m.Units...), m.Services...), m.Timers...) {
		if name == "" || strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, "-") {
			return nil, fmt.Errorf("manifest: invalid unit name %q", name)
		}
	}
	for _, unit := range m.Units {
		if _, err := os.Stat(filepath.Join(dir, "deploy", "systemd", unit)); err != nil {
			return nil, fmt.Errorf("manifest: unit %s is not in the release: %w", unit, err)
		}
	}
	return &m, nil
}

// extract unpacks a gzipped tarball into dest. It accepts only regular
// files and folders with local paths, so a release can't write outside its
// folder or plant symlinks.
func extract(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}

	tr := tar.NewReader(gz)
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(hdr.Name)
		if !filepath.IsLocal(name) {
			return fmt.Errorf("unsafe path %q", hdr.Name)
		}
		target := filepath.Join(dest, name)

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			total += hdr.Size
			if total > maxReleaseBytes {
				return errors.New("release is too large")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			// Keep the executable bit; drop group and world write.
			mode := hdr.FileInfo().Mode().Perm()&0o755 | 0o600
			f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, io.LimitReader(tr, hdr.Size))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported entry %q (type %c)", hdr.Name, hdr.Typeflag)
		}
	}
}
