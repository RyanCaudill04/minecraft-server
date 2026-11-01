package deploy

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/deploy/state"
)

// Track is one part of the Pi that is released and deployed on its own,
// such as play or the agent itself. A track is a repo plus a tag prefix:
// track "play" deploys the play-r<N> releases of Repo, so moving an app to
// its own repo later only changes Repo.
type Track struct {
	Name string // e.g. "play"; its folder is <apps>/<name>
	Repo string // GitHub "owner/name"; "" uses the Agent's Repo
}

var validTrack = regexp.MustCompile(`^[a-z][a-z0-9]{0,31}$`)

// checkTrackName rejects names that aren't plain words, or that clash with
// the single release's own entries in <apps>.
func checkTrackName(name string) error {
	if !validTrack.MatchString(name) || name == "releases" || name == "current" {
		return fmt.Errorf("deploy: invalid track name %q", name)
	}
	return nil
}

// legacyFiles are the single release's state files in <apps>, removed
// once every track runs from its own folder.
var legacyFiles = []string{"failed", "managed.json", "active.json", "activating", "history", "pinned"}

// Agent deploys every track, each with its own Deployer in <apps>/<track>,
// and moves a Pi from the single release to tracks.
//
// Until <apps>/state.json says the layout is state.Tracks, the Pi runs the
// single release in <apps>/current. Each Run then checks whether every
// track has a release it can use, and if so moves to tracks: it installs
// them all, then activates them one at a time (the agent's track last),
// each with its own health checks, relinking each track's units from the
// single release to the track's folder. If any track fails, it puts every
// unit back on the single release, restarts it and records the attempt in
// state.json, so it isn't retried until a track publishes a new release.
// Only once every track is active does it write the Tracks layout. The
// single release's files are removed on a later run, by the agent from the
// agent's track, once no unit links to them.
type Agent struct {
	Apps    string // <apps>, e.g. ~/apps
	Repo    string // repo of the single release, and of tracks without one
	API     string // GitHub API base URL
	UnitDir string // systemd user unit folder

	// Tracks are deployed in this order, except that AgentTrack always
	// goes last: a run that updates the agent then ends, and the timer's
	// next run uses the new agent.
	Tracks []Track
	// AgentTrack is the track that ships mc-agent and its timer.
	AgentTrack string
	// AgentTimer is the timer that runs the agent, e.g. mc-agent.timer.
	AgentTimer string
	// CheckAgent runs "<bin> selfcheck" for the agent at bin, a release's
	// bin/mc-agent. Nil skips the check.
	CheckAgent func(ctx context.Context, bin string) error

	HTTP          *http.Client
	Systemctl     func(ctx context.Context, args ...string) error
	HealthTimeout time.Duration
	Keep          int
	Log           *log.Logger
}

// tracks returns Tracks in deploy order, AgentTrack last.
func (a *Agent) tracks() []Track {
	ts := slices.Clone(a.Tracks)
	slices.SortStableFunc(ts, func(x, y Track) int {
		switch {
		case x.Name == a.AgentTrack && y.Name != a.AgentTrack:
			return 1
		case y.Name == a.AgentTrack && x.Name != a.AgentTrack:
			return -1
		}
		return 0
	})
	return ts
}

func (a *Agent) track(name string) (Track, error) {
	for _, t := range a.Tracks {
		if t.Name == name {
			return t, nil
		}
	}
	var names []string
	for _, t := range a.tracks() {
		names = append(names, t.Name)
	}
	return Track{}, fmt.Errorf("deploy: no track %q; the tracks are %s", name, strings.Join(names, ", "))
}

func (a *Agent) github() *GitHub {
	return &GitHub{API: a.API, HTTP: a.HTTP, Cache: filepath.Join(a.Apps, ".releases-cache.json"), Log: a.Log}
}

// TrackDeployer returns the Deployer of a track's folder, or of the single
// release for "". Its fields are enough to read what is deployed; the
// admin page uses it with only Apps set.
func (a *Agent) TrackDeployer(track string) (*Deployer, error) {
	if track == "" {
		return a.single(nil), nil
	}
	if err := checkTrackName(track); err != nil {
		return nil, err
	}
	t := Track{Name: track}
	if known, err := a.track(track); err == nil {
		t = known
	}
	return a.deployer(t, nil), nil
}

func (a *Agent) deployer(t Track, gh *GitHub) *Deployer {
	repo := t.Repo
	if repo == "" {
		repo = a.Repo
	}
	d := a.base(state.TrackDir(a.Apps, t.Name), repo, gh)
	d.Track = t.Name
	if t.Name == a.AgentTrack {
		d.AgentTimer = a.AgentTimer
		d.CheckAgent = a.checkAgent(d)
	}
	return d
}

// single returns the Deployer of the single release in <apps>.
func (a *Agent) single(gh *GitHub) *Deployer {
	d := a.base(a.Apps, a.Repo, gh)
	d.AgentTimer = a.AgentTimer
	d.CheckAgent = a.checkAgent(d)
	return d
}

func (a *Agent) base(dir, repo string, gh *GitHub) *Deployer {
	return &Deployer{
		Repo: repo, API: a.API, Apps: dir, UnitDir: a.UnitDir,
		GitHub: gh, HTTP: a.HTTP, Systemctl: a.Systemctl,
		HealthTimeout: a.HealthTimeout, Keep: a.Keep, Log: a.logger(),
	}
}

func (a *Agent) logger() *log.Logger {
	if a.Log == nil {
		return log.New(os.Stderr, "", 0)
	}
	return a.Log
}

func (a *Agent) checkAgent(d *Deployer) func(context.Context) error {
	if a.CheckAgent == nil {
		return nil
	}
	return func(ctx context.Context) error {
		return a.CheckAgent(ctx, filepath.Join(d.currentLink(), "bin", "mc-agent"))
	}
}

// lock takes <apps>/.lock, the same lock the single release's agent takes,
// so an old agent run by hand can't overlap with a new one.
func (a *Agent) lock() (func(), error) { return a.single(nil).lock() }

// Run is what the timer calls: it deploys each track's newest release, or
// on the single layout deploys the single release and moves to tracks once
// it can.
func (a *Agent) Run(ctx context.Context) error {
	return a.run(ctx, false)
}

// Migrate is Run, except that it tries the move to tracks again even if
// the same releases failed it before.
func (a *Agent) Migrate(ctx context.Context) error {
	return a.run(ctx, true)
}

func (a *Agent) run(ctx context.Context, force bool) error {
	for _, t := range a.Tracks {
		if err := checkTrackName(t.Name); err != nil {
			return err
		}
	}
	unlock, err := a.lock()
	if err != nil {
		return err
	}
	defer unlock()

	st, err := state.Read(a.Apps)
	if err != nil {
		return err
	}
	gh := a.github()
	if st.Layout == state.Tracks {
		return a.runTracks(ctx, st, gh)
	}

	single := a.single(gh)
	if err := single.resume(ctx); err != nil {
		return err
	}
	moved, err := a.migrate(ctx, st, gh, single, force)
	if moved || err != nil {
		return err
	}
	return single.run(ctx)
}

// runTracks deploys each track on its own. One track failing doesn't stop
// the others.
func (a *Agent) runTracks(ctx context.Context, st *state.State, gh *GitHub) error {
	var errs []error
	var active []string
	for _, t := range a.tracks() {
		d := a.deployer(t, gh)
		err := d.run(ctx)
		if errors.Is(err, ErrNoRelease) {
			// Nothing newer than what is active can be missing from the
			// newest releases, so there is nothing to do.
			if current, _ := d.Current(); current == "" {
				a.logger().Printf("%s: no release published yet", t.Name)
			}
			err = nil
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.Name, err))
		}
		if current, _ := d.Current(); current != "" {
			active = append(active, t.Name)
		}
	}
	// A track added since the move shows up once it has a release.
	if missing := slices.DeleteFunc(active, func(n string) bool { return slices.Contains(st.Tracks, n) }); len(missing) > 0 {
		err := state.Update(a.Apps, func(s *state.State) error {
			for _, n := range missing {
				if !slices.Contains(s.Tracks, n) {
					s.Tracks = append(s.Tracks, n)
				}
			}
			return nil
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		a.removeSingle()
	}
	return errors.Join(errs...)
}

// migrate moves from the single release to tracks if every track has a
// release to move to. It reports whether it tried; if it didn't, the
// caller deploys the single release as before.
func (a *Agent) migrate(ctx context.Context, st *state.State, gh *GitHub, single *Deployer, force bool) (bool, error) {
	l := a.logger()
	current, err := single.Current()
	if err != nil {
		return false, err
	}
	targets, ready, err := a.targets(ctx, gh)
	if err != nil || !ready {
		return false, err
	}

	if current == "" {
		// A new Pi: nothing to move, so start on tracks.
		l.Printf("no release is active; starting with one folder per track")
		if err := a.setTracks(); err != nil {
			return true, err
		}
		st, err := state.Read(a.Apps)
		if err != nil {
			return true, err
		}
		return true, a.runTracks(ctx, st, gh)
	}
	switch {
	case current == readTag(single.failedFile()):
		l.Printf("not moving to tracks: the active release %s failed", current)
		return false, nil
	case readTag(single.pinnedFile()) != "":
		l.Printf("not moving to tracks while %s is pinned; run \"mc-agent unpin\" first", readTag(single.pinnedFile()))
		return false, nil
	}
	tags := map[string]string{}
	for name, rel := range targets {
		tags[name] = rel.Tag
	}
	if !force && st.Migration != nil && maps.Equal(st.Migration.Tags, tags) {
		l.Printf("not moving to tracks: these releases failed it at %s (%s); waiting for new ones, or run \"mc-agent migrate\"",
			st.Migration.At.Format(time.RFC3339), st.Migration.Error)
		return false, nil
	}
	legacy, err := readManifest(single.currentLink())
	if err != nil {
		return false, fmt.Errorf("deploy: %s: %w", current, err)
	}

	// Install everything first: it changes nothing that runs.
	var ds []*Deployer
	for _, t := range a.tracks() {
		d := a.deployer(t, gh)
		if err := d.Install(ctx, targets[t.Name]); err != nil {
			err = fmt.Errorf("installing %s: %w", targets[t.Name].Tag, err)
			a.recordFailure(tags, err)
			l.Printf("not moving to tracks: %v; staying on %s", err, current)
			return false, nil
		}
		d.Adopt = []string{single.currentLink(), single.releasesDir()}
		ds = append(ds, d)
	}

	l.Printf("moving from %s to one folder per track", current)
	for i, d := range ds {
		if err := d.activate(ctx, tags[d.Track], true); err != nil {
			l.Printf("moving to tracks: %s failed; putting every unit back on %s", d.Track, current)
			rbErr := a.revert(ctx, single, legacy, ds[:i+1])
			a.recordFailure(tags, fmt.Errorf("%s: %w", d.Track, err))
			if rbErr != nil {
				rbErr = fmt.Errorf("putting the units back on %s: %w", current, rbErr)
			}
			return true, errors.Join(fmt.Errorf("deploy: moving to tracks: %s: %w", d.Track, err), rbErr)
		}
	}
	if err := a.setTracks(); err != nil {
		return true, err
	}
	l.Printf("every track is active; %s is removed on a later run", current)
	return true, nil
}

// targets returns each track's newest release, and whether every track
// has one that hasn't failed.
func (a *Agent) targets(ctx context.Context, gh *GitHub) (map[string]*Release, bool, error) {
	if len(a.Tracks) == 0 {
		return nil, false, nil
	}
	out := map[string]*Release{}
	for _, t := range a.tracks() {
		d := a.deployer(t, gh)
		rel, err := d.Latest(ctx)
		if errors.Is(err, ErrNoRelease) {
			a.logger().Printf("not moving to tracks yet: %s has no release", t.Name)
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		if d.hasFailed(rel.Tag) {
			a.logger().Printf("not moving to tracks yet: %s failed; waiting for a newer %s release", rel.Tag, t.Name)
			return nil, false, nil
		}
		out[t.Name] = rel
	}
	return out, true, nil
}

// revert puts every unit back on the single release after the move to
// tracks failed partway. done are the tracks that were activated.
func (a *Agent) revert(ctx context.Context, single *Deployer, legacy *Manifest, done []*Deployer) error {
	for _, d := range done {
		// Units only a track has: the single release won't relink them.
		if m, err := readManifest(d.currentLink()); err == nil {
			d.remove(ctx, minus(m, legacy))
		}
	}
	err := single.apply(ctx, false)
	for _, d := range done {
		d.abandon()
	}
	return err
}

// abandon makes a track folder read as having nothing active, after the
// move to tracks was undone. Installed releases, the history and the
// failed marker stay, so a release that failed isn't tried again.
func (d *Deployer) abandon() {
	for _, path := range []string{d.currentLink(), d.managedFile(), d.appliedFile(), d.pendingFile()} {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			d.Log.Printf("removing %s: %v", path, err)
		}
	}
}

func (a *Agent) recordFailure(tags map[string]string, cause error) {
	err := state.Update(a.Apps, func(s *state.State) error {
		s.Migration = &state.Migration{Tags: tags, Error: cause.Error(), At: time.Now().UTC()}
		return nil
	})
	if err != nil {
		a.logger().Printf("recording the failed move to tracks: %v", err)
	}
}

// setTracks records the Tracks layout. From here on, Run deploys tracks.
func (a *Agent) setTracks() error {
	var names []string
	for _, t := range a.tracks() {
		names = append(names, t.Name)
	}
	return state.Update(a.Apps, func(s *state.State) error {
		s.Layout = state.Tracks
		s.Tracks = names
		s.Migration = nil
		return nil
	})
}

// removeSingle removes the single release's folder, link and files once
// no unit links into them any more.
func (a *Agent) removeSingle() {
	single := a.single(nil)
	_, linkErr := os.Lstat(single.currentLink())
	_, dirErr := os.Stat(single.releasesDir())
	if linkErr != nil && dirErr != nil {
		return
	}
	entries, _ := os.ReadDir(a.UnitDir)
	for _, e := range entries {
		path := filepath.Join(a.UnitDir, e.Name())
		if single.linksInto(path, single.currentLink()) || single.linksInto(path, single.releasesDir()) {
			a.logger().Printf("keeping the single release: %s still links to it", path)
			return
		}
	}
	a.logger().Printf("removing the single release, now that every track runs from its own folder")
	paths := []string{single.currentLink(), single.releasesDir()}
	for _, name := range legacyFiles {
		paths = append(paths, filepath.Join(a.Apps, name))
	}
	for _, path := range paths {
		if err := os.RemoveAll(path); err != nil {
			a.logger().Printf("removing %s: %v", path, err)
		}
	}
}

// Rollback rolls a track back and pins it (see Deployer.Rollback). On the
// single layout, track must be "".
func (a *Agent) Rollback(ctx context.Context, track string) error {
	return a.withTrack(track, func(d *Deployer) error { return d.rollback(ctx) })
}

// Unpin lets a track follow its latest release again. On the single
// layout, track must be "".
func (a *Agent) Unpin(track string) error {
	return a.withTrack(track, func(d *Deployer) error { return d.unpinCmd() })
}

func (a *Agent) withTrack(track string, f func(*Deployer) error) error {
	unlock, err := a.lock()
	if err != nil {
		return err
	}
	defer unlock()
	st, err := state.Read(a.Apps)
	if err != nil {
		return err
	}
	if st.Layout != state.Tracks {
		if track != "" {
			return errors.New("deploy: this Pi still runs the single release, so leave out the track")
		}
		return f(a.single(nil))
	}
	if track == "" {
		_, err := a.track("")
		return fmt.Errorf("deploy: name a track: %w", err)
	}
	t, err := a.track(track)
	if err != nil {
		return err
	}
	return f(a.deployer(t, nil))
}

// SelfCheck is "mc-agent selfcheck" (see Deployer.SelfCheck). version tells
// which release the agent came from: an agent built for the agent's track
// (agent-r<N>) checks that track, any other the single release.
func (a *Agent) SelfCheck(ctx context.Context, version string) error {
	if a.AgentTrack != "" && strings.HasPrefix(version, a.AgentTrack+"-") {
		t, err := a.track(a.AgentTrack)
		if err != nil {
			return err
		}
		return a.deployer(t, nil).SelfCheck(ctx, version)
	}
	return a.single(nil).SelfCheck(ctx, version)
}

// Status is what is deployed, per track. It only reads.
type Status struct {
	Layout state.Layout  `json:"layout"`
	Tracks []TrackStatus `json:"tracks"`
}

// TrackStatus is one track's state. Track is "" for the single release.
type TrackStatus struct {
	Track      string `json:"track"`
	Active     string `json:"active"`              // tag in current, "" if none
	Failed     string `json:"failed,omitzero"`     // tag marked failed
	Pinned     string `json:"pinned,omitzero"`     // tag pinned by a rollback
	Activating string `json:"activating,omitzero"` // tag whose activation hasn't finished
}

// Status reads what each track has deployed. It fails on a state.json
// newer than this program knows.
func (a *Agent) Status() (*Status, error) {
	st, err := state.Read(a.Apps)
	if err != nil {
		return nil, err
	}
	s := &Status{Layout: st.Layout}
	names := []string{""}
	if st.Layout == state.Tracks {
		names = st.Tracks
	}
	for _, name := range names {
		d, err := a.TrackDeployer(name)
		if err != nil {
			return nil, err
		}
		current, err := d.Current()
		if err != nil {
			return nil, err
		}
		s.Tracks = append(s.Tracks, TrackStatus{
			Track:      name,
			Active:     current,
			Failed:     readTag(d.failedFile()),
			Pinned:     readTag(d.pinnedFile()),
			Activating: readTag(d.pendingFile()),
		})
	}
	return s, nil
}
