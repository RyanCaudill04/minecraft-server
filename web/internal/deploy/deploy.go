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
//
// Units a release no longer lists are stopped, disabled and unlinked before
// current switches. The agent records what it manages in
// <apps>/managed.json, so this also works after a rollback, for template
// instances such as site@admin.service whose template is still shipped, and
// on the deploy after one that was interrupted.
//
// Each switch of current is recorded in <apps>/active.json with its time,
// so the admin page can show when the active release was applied.
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
	"slices"
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

// NotesPath is where a release's notes live inside it. Releases built
// before notes were added don't have them.
const NotesPath = "deploy/notes.json"

// Limits on what a release may contain.
const (
	maxReleaseBytes = 200 << 20
	maxChecksumSize = 1 << 10
	maxNotesSize    = 1 << 20
)

var validTag = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Manifest says how to activate a release. It is deploy/release.json.
type Manifest struct {
	Units    []string `json:"units"`    // files in deploy/systemd to link into the user unit folder
	Services []string `json:"services"` // enabled and restarted on every deploy
	Timers   []string `json:"timers"`   // enabled and started
	Health   []string `json:"health"`   // URLs that must answer 200 after a deploy
}

// Applied records which release current was last switched to, and when.
// It is <apps>/active.json.
type Applied struct {
	Tag string    `json:"tag"`
	At  time.Time `json:"applied_at"`
}

// Notes describe what changed in a release. They are deploy/notes.json,
// written by scripts/release-notes.sh.
type Notes struct {
	Tag        string    `json:"tag"`
	Kind       string    `json:"kind"` // feature, fix or other
	ReleasedAt time.Time `json:"released_at"`
	Changes    []Change  `json:"changes"`
}

// Change is one PR in a release, or a commit pushed without one.
type Change struct {
	PR    int    `json:"pr,omitzero"` // 0 for a commit pushed without a PR
	Title string `json:"title"`
	Kind  string `json:"kind"` // feature, fix or other
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
func (d *Deployer) managedFile() string { return filepath.Join(d.Apps, "managed.json") }
func (d *Deployer) appliedFile() string { return filepath.Join(d.Apps, "active.json") }

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

// Notes returns the active release's notes, or nil if nothing is active or
// the release has none.
func (d *Deployer) Notes() (*Notes, error) {
	f, err := os.Open(filepath.Join(d.currentLink(), NotesPath))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var n Notes
	if err := json.NewDecoder(io.LimitReader(f, maxNotesSize)).Decode(&n); err != nil {
		return nil, fmt.Errorf("deploy: reading %s: %w", NotesPath, err)
	}
	return &n, nil
}

// LastApplied returns when the active release was switched to, or nil if
// that isn't recorded: nothing is active, or an agent from before
// active.json was added switched to it.
func (d *Deployer) LastApplied() (*Applied, error) {
	current, err := d.Current()
	if err != nil || current == "" {
		return nil, err
	}
	data, err := os.ReadFile(d.appliedFile())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var a Applied
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("deploy: reading %s: %w", d.appliedFile(), err)
	}
	if a.Tag != current {
		return nil, nil
	}
	return &a, nil
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
	next, err := readManifest(filepath.Join(d.releasesDir(), tag))
	if err != nil {
		return fmt.Errorf("deploy: %s: %w", tag, err)
	}
	// While current still points at the release that has the unit files,
	// so they can be disabled.
	if err := d.retire(ctx, next); err != nil {
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
		rbErr := d.rollBackTo(ctx, previous)
		if rbErr != nil {
			return errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
		}
		return fmt.Errorf("deploy: %s failed and was rolled back to %s: %w", tag, previous, err)
	}

	os.Remove(d.failedFile())
	d.Log.Printf("%s is active", tag)
	return d.prune()
}

// rollBackTo reactivates tag after the current release failed, first
// removing whatever the failed release added.
func (d *Deployer) rollBackTo(ctx context.Context, tag string) error {
	m, err := readManifest(filepath.Join(d.releasesDir(), tag))
	if err != nil {
		return err
	}
	if err := d.retire(ctx, m); err != nil {
		return err
	}
	if err := d.switchTo(tag); err != nil {
		return err
	}
	return d.apply(ctx)
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
	if err := os.Rename(tmp, d.currentLink()); err != nil {
		return err
	}
	// Only a record for the admin page: current has already switched, so
	// failing to write it mustn't fail the deploy.
	data, err := json.Marshal(Applied{Tag: tag, At: time.Now().UTC()})
	if err == nil {
		err = d.writeFile(d.appliedFile(), data)
	}
	if err != nil {
		d.Log.Printf("recording when %s was applied: %v", tag, err)
	}
	return nil
}

// retire stops, disables and unlinks what the agent manages now that next
// doesn't list. It runs before current switches to next, while the unit
// files still exist: systemctl can't disable a unit whose file is gone.
// It records the union of both first, so a deploy interrupted after this
// still leaves a list the next deploy can clean up from.
func (d *Deployer) retire(ctx context.Context, next *Manifest) error {
	old := d.readManaged()
	if old == nil {
		// managed.json is missing (an agent from before it was added) or
		// unreadable: fall back to what the current release lists.
		if _, err := os.Lstat(d.currentLink()); err == nil {
			if old, err = readManifest(d.currentLink()); err != nil {
				d.Log.Printf("can't tell what the current release runs, so nothing is removed: %v", err)
			}
		}
	}
	if err := d.writeManaged(union(old, next)); err != nil {
		return err
	}
	if old != nil {
		// Before anything restarts: a removed service may hold a port that
		// a new one needs.
		d.remove(ctx, minus(old, next))
	}
	return nil
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
		if _, err := os.Lstat(link); err == nil && !d.linksInto(link, d.Apps) {
			return fmt.Errorf("deploy: %s was installed by hand; remove it to let the release manage %s", link, unit)
		}
		os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			return err
		}
	}

	// After the links above, so a template is in place before an
	// instance's own link stops hiding it. The enable below then moves the
	// instance's enable links to the release too.
	if err := d.unshadow(m); err != nil {
		return err
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
	return d.writeManaged(m)
}

// remove stops, disables and unlinks r's units. It only touches units
// whose unit file (or template, for an instance such as site@admin.service)
// is linked into Apps, so units installed by hand, such as
// minecraft.service, are never stopped or unlinked even if a release once
// listed them. Failures are logged, not returned: a service that really is
// still running fails the next health check instead.
func (d *Deployer) remove(ctx context.Context, r *Manifest) {
	var stop []string
	for _, unit := range slices.Concat(r.Services, r.Timers) {
		if !d.linksInto(filepath.Join(d.UnitDir, unitFile(unit)), d.Apps) {
			d.Log.Printf("keeping %s: the release doesn't provide it", unit)
			continue
		}
		d.Log.Printf("removing %s: no longer in the release", unit)
		stop = append(stop, unit)
	}
	if len(stop) > 0 {
		if err := d.Systemctl(ctx, append([]string{"disable", "--now"}, stop...)...); err != nil {
			// One bad unit fails the whole call, so retry each on its own.
			d.Log.Printf("systemctl disable --now: %v; retrying one at a time", err)
			for _, unit := range stop {
				if err := d.Systemctl(ctx, "disable", "--now", unit); err != nil {
					d.Log.Printf("systemctl disable --now %s: %v", unit, err)
				}
			}
		}
	}
	for _, unit := range r.Units {
		link := filepath.Join(d.UnitDir, unit)
		if _, err := os.Lstat(link); err != nil {
			continue
		}
		if !d.linksInto(link, d.Apps) {
			d.Log.Printf("keeping %s: it wasn't linked by the agent", link)
			continue
		}
		d.Log.Printf("unlinking %s", unit)
		if err := os.Remove(link); err != nil {
			d.Log.Printf("unlinking %s: %v", unit, err)
		}
	}
}

// unshadow removes links in the unit folder named after one of m's
// template instances, such as site@portfolio.service, when the release
// ships the template. systemd prefers a unit file with the instance's exact
// name over the template, so such a link (left from installing by hand)
// would keep the service running from wherever it points instead of the
// release. A regular file in that place fails the deploy: it was written on
// purpose, and the release can't run the service while it's there.
func (d *Deployer) unshadow(m *Manifest) error {
	for _, name := range slices.Concat(m.Services, m.Timers) {
		template := unitFile(name)
		if template == name || !slices.Contains(m.Units, template) || slices.Contains(m.Units, name) {
			continue // not an instance of a template the release ships
		}
		path := filepath.Join(d.UnitDir, name)
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			return fmt.Errorf("deploy: %s overrides the release's %s; remove it to let the release run %s", path, template, name)
		}
		if d.linksInto(path, d.currentLink()) {
			continue // the release's own file, by another path
		}
		target, _ := os.Readlink(path)
		d.Log.Printf("unlinking %s: it points at %s instead of the release's %s", path, target, template)
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

// linksInto reports whether path is a symlink to somewhere inside dir.
// Relative targets are resolved from the link's folder.
func (d *Deployer) linksInto(path, dir string) bool {
	target, err := os.Readlink(path)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	rel, err := filepath.Rel(dir, target)
	return err == nil && filepath.IsLocal(rel)
}

// unitFile returns the file a unit is loaded from: the template for an
// instance (site@admin.service -> site@.service), else the unit itself.
func unitFile(unit string) string {
	if at := strings.Index(unit, "@"); at >= 0 {
		if dot := strings.LastIndex(unit, "."); dot > at {
			return unit[:at+1] + unit[dot:]
		}
	}
	return unit
}

// readManaged returns the units, services and timers the agent last
// recorded, or nil if it hasn't recorded any or the record is unreadable.
// An unreadable record is logged rather than returned, so it can't block
// every deploy (and every rollback) until someone deletes it by hand.
func (d *Deployer) readManaged() *Manifest {
	data, err := os.ReadFile(d.managedFile())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	var m Manifest
	if err == nil {
		err = json.Unmarshal(data, &m)
	}
	if err == nil {
		err = checkNames(&m)
	}
	if err != nil {
		d.Log.Printf("ignoring %s: %v", d.managedFile(), err)
		return nil
	}
	return &m
}

// writeManaged records m in managed.json.
func (d *Deployer) writeManaged(m *Manifest) error {
	data, err := json.Marshal(Manifest{Units: m.Units, Services: m.Services, Timers: m.Timers})
	if err != nil {
		return err
	}
	return d.writeFile(d.managedFile(), data)
}

// writeFile replaces path, a file in Apps, atomically and durably, so a
// power cut can't leave it half written.
func (d *Deployer) writeFile(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if dir, err := os.Open(d.Apps); err == nil {
		dir.Sync()
		dir.Close()
	}
	return nil
}

// union returns the units, services and timers in any of ms, without
// duplicates. Nil manifests are skipped.
func union(ms ...*Manifest) *Manifest {
	var u Manifest
	for _, m := range ms {
		if m == nil {
			continue
		}
		u.Units = appendNew(u.Units, m.Units)
		u.Services = appendNew(u.Services, m.Services)
		u.Timers = appendNew(u.Timers, m.Timers)
	}
	return &u
}

// minus returns the units, services and timers in a but not in b.
func minus(a, b *Manifest) *Manifest {
	without := func(xs, drop []string) []string {
		var out []string
		for _, x := range xs {
			if !slices.Contains(drop, x) {
				out = append(out, x)
			}
		}
		return out
	}
	return &Manifest{
		Units:    without(a.Units, b.Units),
		Services: without(a.Services, b.Services),
		Timers:   without(a.Timers, b.Timers),
	}
}

func appendNew(xs, add []string) []string {
	for _, x := range add {
		if !slices.Contains(xs, x) {
			xs = append(xs, x)
		}
	}
	return xs
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
	if err := checkNames(&m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	for _, unit := range m.Units {
		if _, err := os.Stat(filepath.Join(dir, "deploy", "systemd", unit)); err != nil {
			return nil, fmt.Errorf("manifest: unit %s is not in the release: %w", unit, err)
		}
	}
	return &m, nil
}

// checkNames rejects unit names that could be read as a path or a flag.
func checkNames(m *Manifest) error {
	for _, name := range append(append(append([]string{}, m.Units...), m.Services...), m.Timers...) {
		if name == "" || strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, "-") {
			return fmt.Errorf("invalid unit name %q", name)
		}
	}
	return nil
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
