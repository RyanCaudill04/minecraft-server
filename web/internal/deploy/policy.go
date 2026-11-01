package deploy

// Update policies (#83) and the changes admin asks the agent to make.
//
// The update policy says what Run does with a newer release:
//
//   - auto: deploy it. This is the default, and how the agent always worked.
//   - notify: record it in <apps>/latest.json, so admin can show it, but
//     don't install it.
//   - pinned: stay on one release. The pin is <apps>/pinned, the same file
//     Rollback writes, so a rollback is just a pin to the previous release.
//
// <apps>/policy.json holds the mode for each track and a global pause, which
// holds every track (an interrupted deploy is still finished):
//
//	{"version": 1, "paused": false, "tracks": {"all": {"mode": "notify"}}}
//
// Today one release holds every app, so there is one track, Track. When
// releases are split per track (#79), each gets its own entry here and its
// own pin, and the rest of this file stays the same. A pinned track is never
// written to policy.json: the pin file is the only record of it, so the
// pin can't disagree with the policy.
//
// A pinned release that fails is unpinned and the policy switches to
// notify, so the timer neither retries it nor jumps to a release nobody
// chose. Only releases whose manifest says they follow policies can be
// pinned (Manifest.Policy): the timer runs the active release's agent, and
// an older one would ignore the pin and deploy the latest release.
//
// admin can't deploy itself, because a deploy restarts it partway through.
// It writes what it wants to <apps>/request.json with Submit and starts
// mc-agent.service; the next Run carries it out under the agent's lock and
// records the outcome in <apps>/request-result.json.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// PolicyVersion is the version of policy.json this agent reads and writes,
// and what a release's manifest sets "policy" to when its agent follows it.
const PolicyVersion = 1

// Track is the only track today: the release that holds every app.
const Track = "all"

// Mode is a track's update policy.
type Mode string

const (
	ModeAuto   Mode = "auto"   // deploy the newest release
	ModeNotify Mode = "notify" // record the newest release, but don't install it
	ModePinned Mode = "pinned" // stay on one release
)

// TrackPolicy is one track's update policy.
type TrackPolicy struct {
	Mode Mode   `json:"mode"`
	Tag  string `json:"tag,omitzero"` // the pinned release, for ModePinned
}

// Policy is <apps>/policy.json, with each track's pin filled in.
type Policy struct {
	Version int                    `json:"version"`
	Paused  bool                   `json:"paused,omitzero"`
	Tracks  map[string]TrackPolicy `json:"tracks,omitzero"`
}

// Mode returns track's mode, auto unless something else is set.
func (p *Policy) Mode(track string) Mode {
	switch m := p.Tracks[track].Mode; m {
	case ModeNotify, ModePinned:
		return m
	}
	return ModeAuto
}

// ErrUnsupportedPolicy means policy.json was written by a newer agent. The
// agent then deploys as if there were no policy file (so a fix can still
// arrive) and refuses to change it.
var ErrUnsupportedPolicy = errors.New("deploy: policy.json is from a newer mc-agent")

func (d *Deployer) policyFile() string        { return filepath.Join(d.Apps, "policy.json") }
func (d *Deployer) latestFile() string        { return filepath.Join(d.Apps, "latest.json") }
func (d *Deployer) requestFile() string       { return filepath.Join(d.Apps, "request.json") }
func (d *Deployer) requestResultFile() string { return filepath.Join(d.Apps, "request-result.json") }

// storedPolicy reads policy.json as written, without the pin. A missing
// file is the default policy.
func (d *Deployer) storedPolicy() (*Policy, error) {
	p := &Policy{Version: PolicyVersion}
	data, err := os.ReadFile(d.policyFile())
	if errors.Is(err, fs.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, p); err != nil {
		return nil, fmt.Errorf("deploy: reading %s: %w", d.policyFile(), err)
	}
	if p.Version > PolicyVersion {
		return nil, fmt.Errorf("%w (version %d; this one reads %d)", ErrUnsupportedPolicy, p.Version, PolicyVersion)
	}
	p.Version = PolicyVersion
	return p, nil
}

// ReadPolicy returns the update policy, including the pin. It only reads,
// so admin can call it.
func (d *Deployer) ReadPolicy() (*Policy, error) {
	p, err := d.storedPolicy()
	if err != nil {
		return nil, err
	}
	if p.Tracks == nil {
		p.Tracks = map[string]TrackPolicy{}
	}
	if pinned := readTag(d.pinnedFile()); pinned != "" {
		p.Tracks[Track] = TrackPolicy{Mode: ModePinned, Tag: pinned}
	} else {
		p.Tracks[Track] = TrackPolicy{Mode: p.Mode(Track)}
		if p.Tracks[Track].Mode == ModePinned { // written by hand; only the pin file pins
			p.Tracks[Track] = TrackPolicy{Mode: ModeAuto}
		}
	}
	return p, nil
}

// policy is ReadPolicy for Run: a policy that can't be read is logged and
// taken as the default, so it can't stop a fix from being deployed.
func (d *Deployer) policy() *Policy {
	p, err := d.ReadPolicy()
	if err != nil {
		d.Log.Printf("ignoring the update policy: %v", err)
		return &Policy{Version: PolicyVersion}
	}
	return p
}

// writePolicy replaces policy.json with p, leaving out pins.
func (d *Deployer) writePolicy(p *Policy) error {
	out := Policy{Version: PolicyVersion, Paused: p.Paused, Tracks: map[string]TrackPolicy{}}
	for track, tp := range p.Tracks {
		if tp.Mode == ModeAuto || tp.Mode == ModeNotify {
			out.Tracks[track] = TrackPolicy{Mode: tp.Mode}
		}
	}
	data, err := json.Marshal(out)
	if err != nil {
		return err
	}
	return d.writeFile(d.policyFile(), append(data, '\n'))
}

// storeMode sets Track's mode in policy.json, auto or notify.
func (d *Deployer) storeMode(mode Mode) error {
	p, err := d.storedPolicy()
	if err != nil {
		return err
	}
	if p.Tracks == nil {
		p.Tracks = map[string]TrackPolicy{}
	}
	p.Tracks[Track] = TrackPolicy{Mode: mode}
	return d.writePolicy(p)
}

// SetMode sets the update policy to auto or notify, unpinning the release
// if one is pinned. "mc-agent policy auto|notify".
func (d *Deployer) SetMode(mode Mode) error {
	unlock, err := d.lock()
	if err != nil {
		return err
	}
	defer unlock()
	return d.setMode(mode)
}

func (d *Deployer) setMode(mode Mode) error {
	if mode != ModeAuto && mode != ModeNotify {
		return fmt.Errorf("deploy: the policy must be auto or notify, not %q; pin a release to stay on it", mode)
	}
	if err := d.storeMode(mode); err != nil {
		return err
	}
	d.Log.Printf("the update policy is now %s", mode)
	return d.unpin()
}

// SetPaused pauses or resumes every update. "mc-agent pause|resume".
func (d *Deployer) SetPaused(paused bool) error {
	unlock, err := d.lock()
	if err != nil {
		return err
	}
	defer unlock()
	return d.setPaused(paused)
}

func (d *Deployer) setPaused(paused bool) error {
	p, err := d.storedPolicy()
	if err != nil {
		return err
	}
	p.Paused = paused
	if err := d.writePolicy(p); err != nil {
		return err
	}
	if paused {
		d.Log.Printf("updates are paused")
	} else {
		d.Log.Printf("updates are resumed")
	}
	return nil
}

// Pin makes tag the active release and keeps it there until the policy
// changes. It installs tag first if needed. "mc-agent pin <tag>".
func (d *Deployer) Pin(ctx context.Context, tag string) error {
	unlock, err := d.lock()
	if err != nil {
		return err
	}
	defer unlock()
	return d.pin(ctx, tag)
}

func (d *Deployer) pin(ctx context.Context, tag string) error {
	if !validTag.MatchString(tag) {
		return fmt.Errorf("deploy: invalid release tag %q", tag)
	}
	current, err := d.Current()
	if err != nil {
		return err
	}
	dir := filepath.Join(d.releasesDir(), tag)
	if _, err := readManifest(dir); err != nil {
		rel, err := d.Release(ctx, tag)
		if err != nil {
			return err
		}
		if err := d.Install(ctx, rel); err != nil {
			return err
		}
	}
	m, err := readManifest(dir)
	if err != nil {
		return fmt.Errorf("deploy: %s: %w", tag, err)
	}
	if m.Policy < PolicyVersion {
		return fmt.Errorf("deploy: %s is from before update policies, so its mc-agent would ignore the pin and deploy the latest release; pick a newer one", tag)
	}

	// Before switching, like Rollback, so an interrupted switch is finished
	// by the next run instead of undone.
	if err := d.writeFile(d.pinnedFile(), []byte(tag+"\n")); err != nil {
		return err
	}
	d.Log.Printf("pinned to %s: the timer won't deploy other releases until the policy changes", tag)
	if tag == current {
		return nil
	}
	// A release that never passed a deploy here gets every check a new one
	// does, its agent's selfcheck included.
	if err := d.activate(ctx, tag, !d.deployed(tag)); err != nil {
		if readTag(d.pinnedFile()) == tag {
			d.unpin()
		}
		return err
	}
	return d.prune()
}

// Seen is <apps>/latest.json: the newest release the agent last saw on
// GitHub, and when it looked.
type Seen struct {
	Tag     string    `json:"tag"`
	Checked time.Time `json:"checked_at"`
}

// recordLatest records tag as the newest release. A failure is only
// logged: it's for admin to show.
func (d *Deployer) recordLatest(tag string) {
	data, err := json.Marshal(Seen{Tag: tag, Checked: time.Now().UTC()})
	if err == nil {
		err = d.writeFile(d.latestFile(), data)
	}
	if err != nil {
		d.Log.Printf("recording the latest release: %v", err)
	}
}

// LatestSeen returns the newest release the agent last saw, or nil if it
// hasn't recorded one.
func (d *Deployer) LatestSeen() (*Seen, error) {
	var s Seen
	if found, err := readJSON(d.latestFile(), &s); !found || err != nil {
		return nil, err
	}
	if !validTag.MatchString(s.Tag) {
		return nil, fmt.Errorf("deploy: %s holds an invalid tag", d.latestFile())
	}
	return &s, nil
}

// InstalledRelease is one release in <apps>/releases.
type InstalledRelease struct {
	Tag         string    `json:"tag"`
	InstalledAt time.Time `json:"installed_at"`
	Policy      bool      `json:"policy"`   // its agent follows update policies, so it can be pinned
	Deployed    bool      `json:"deployed"` // it last passed a deploy here
	Failed      bool      `json:"failed"`   // it is marked failed, or last failed a deploy
}

// InstalledReleases lists the installed releases, newest first. It only
// reads.
func (d *Deployer) InstalledReleases() ([]InstalledRelease, error) {
	tags, err := d.installed()
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]InstalledRelease, 0, len(tags))
	for _, tag := range tags {
		dir := filepath.Join(d.releasesDir(), tag)
		r := InstalledRelease{Tag: tag, Deployed: d.deployed(tag), Failed: d.hasFailed(tag)}
		if info, err := os.Stat(dir); err == nil {
			r.InstalledAt = info.ModTime().UTC()
		}
		if m, err := readManifest(dir); err == nil {
			r.Policy = m.Policy >= PolicyVersion
		}
		out = append(out, r)
	}
	return out, nil
}

// Request actions.
const (
	ActionAuto   = "auto"   // follow the latest release
	ActionNotify = "notify" // only record newer releases
	ActionPin    = "pin"    // apply Tag and stay on it
	ActionPause  = "pause"  // hold every update
	ActionResume = "resume" // stop holding them
)

// RequestMaxAge is how long a request waits for the agent. An older one
// is dropped instead of carried out by surprise, and admin may replace it.
const RequestMaxAge = 10 * time.Minute

// ErrBusy means a change admin asked for hasn't been carried out yet.
var ErrBusy = errors.New("deploy: mc-agent hasn't carried out the last change yet")

// Request is a change admin asks the agent to make: <apps>/request.json.
type Request struct {
	Action string    `json:"action"`
	Track  string    `json:"track,omitzero"` // "" or Track
	Tag    string    `json:"tag,omitzero"`   // for ActionPin
	By     string    `json:"by"`             // the signed-in user who asked
	At     time.Time `json:"at"`
}

// Check reports what is wrong with r, if anything.
func (r *Request) Check() error {
	switch r.Action {
	case ActionAuto, ActionNotify, ActionPause, ActionResume:
		if r.Tag != "" {
			return fmt.Errorf("deploy: %s takes no tag", r.Action)
		}
	case ActionPin:
		if !validTag.MatchString(r.Tag) {
			return fmt.Errorf("deploy: invalid release tag %q", r.Tag)
		}
	default:
		return fmt.Errorf("deploy: unknown action %q", r.Action)
	}
	if r.Track != "" && r.Track != Track {
		return fmt.Errorf("deploy: unknown track %q", r.Track)
	}
	// Logged as is, so it mustn't be able to forge a log line.
	if r.By == "" || len(r.By) > 254 || strings.ContainsFunc(r.By, func(c rune) bool {
		return unicode.IsSpace(c) || !unicode.IsPrint(c)
	}) {
		return fmt.Errorf("deploy: invalid user %q", r.By)
	}
	if r.At.IsZero() {
		return errors.New("deploy: the request has no time")
	}
	return nil
}

// RequestResult is <apps>/request-result.json: what became of the last
// request.
type RequestResult struct {
	Request
	Done  time.Time `json:"done_at"`
	Error string    `json:"error,omitzero"`
}

// Submit asks the agent to make a change on its next run. It refuses with
// ErrBusy while an earlier request younger than RequestMaxAge is waiting.
// Only admin calls it, and only one at a time.
func (d *Deployer) Submit(r Request) error {
	if err := r.Check(); err != nil {
		return err
	}
	if p, err := d.PendingRequest(); err != nil {
		return err
	} else if p != nil && time.Since(p.At) < RequestMaxAge {
		return ErrBusy
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return d.writeFile(d.requestFile(), data)
}

// PendingRequest returns the request waiting for the agent, or nil.
func (d *Deployer) PendingRequest() (*Request, error) {
	var r Request
	if found, err := readJSON(d.requestFile(), &r); !found || err != nil {
		return nil, err
	}
	return &r, nil
}

// LastRequest returns what became of the last request, or nil.
func (d *Deployer) LastRequest() (*RequestResult, error) {
	var r RequestResult
	if found, err := readJSON(d.requestResultFile(), &r); !found || err != nil {
		return nil, err
	}
	return &r, nil
}

// handleRequest carries out the waiting request, if there is one. Only a
// failure to carry out a valid request is returned; a malformed or expired
// one is dropped and recorded, and the run goes on.
func (d *Deployer) handleRequest(ctx context.Context) error {
	data, err := os.ReadFile(d.requestFile())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	// Removed first, so a request that kills the agent isn't retried on
	// every run. A pin that is cut short is finished from the pin file.
	if err := os.Remove(d.requestFile()); err != nil {
		return err
	}
	var r Request
	err = json.Unmarshal(data, &r)
	if err == nil {
		err = r.Check()
	}
	if err == nil && time.Since(r.At) > RequestMaxAge {
		err = fmt.Errorf("deploy: the request from %s expired before mc-agent ran", r.At.Format(time.RFC3339))
	}
	if err != nil {
		d.Log.Printf("dropping admin's request: %v", err)
		d.recordRequest(r, err)
		return nil
	}

	what := r.Action
	if r.Action == ActionPin {
		what += " " + r.Tag
	}
	d.Log.Printf("%s asked for %s in admin", r.By, what)
	switch r.Action {
	case ActionAuto:
		err = d.setMode(ModeAuto)
	case ActionNotify:
		err = d.setMode(ModeNotify)
	case ActionPin:
		err = d.pin(ctx, r.Tag)
	case ActionPause:
		err = d.setPaused(true)
	case ActionResume:
		err = d.setPaused(false)
	}
	d.recordRequest(r, err)
	if err != nil {
		return fmt.Errorf("deploy: %s asked for %s: %w", r.By, what, err)
	}
	return nil
}

func (d *Deployer) recordRequest(r Request, err error) {
	res := RequestResult{Request: r, Done: time.Now().UTC()}
	if err != nil {
		res.Error = err.Error()
	}
	data, merr := json.Marshal(res)
	if merr == nil {
		merr = d.writeFile(d.requestResultFile(), data)
	}
	if merr != nil {
		d.Log.Printf("recording the outcome of admin's request: %v", merr)
	}
}

// readJSON decodes the file at path into v. A missing file is found=false
// with no error.
func readJSON(path string, v any) (found bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return false, fmt.Errorf("deploy: reading %s: %w", path, err)
	}
	return true, nil
}
