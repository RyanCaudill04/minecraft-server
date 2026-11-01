// Package state owns <apps>/state.json, the deploy state that mc-agent
// writes and the admin page reads. The two ship in different tracks, so
// they can be at different versions; this file is the contract between
// them.
//
// # Layout
//
// A Pi without state.json uses the layout from before tracks (version 0):
// one release in <apps>/current, a link into <apps>/releases/<tag>, plus
// the agent's files next to it (failed, activating, history, pinned,
// managed.json, active.json).
//
// With layout "tracks", each name in Tracks has its own folder,
// <apps>/<track>, holding the same files as the old layout did for the
// whole Pi:
//
//	releases/<tag>/  installed releases of the track
//	current          link to the active one, releases/<tag>
//	active.json      {"tag", "applied_at"}: when current last switched
//	failed           a tag that failed to deploy, so it isn't retried
//	pinned           a tag "mc-agent rollback" pinned
//	activating       a tag being activated; set until it passes its checks
//	history          "<tag> ok|failed" lines, newest first
//	managed.json     the units the agent manages for the track
//
// The release in current carries deploy/release.json (its manifest) and,
// for releases built with notes, deploy/notes.json. Readers other than
// mc-agent should only read current, active.json, failed and pinned, and
// treat a missing file as "not set".
//
// # Versions
//
// Version is the version of this format. Readers ignore fields they don't
// know, so adding a field doesn't change it. Bump it when a reader that
// knows only the older version would do the wrong thing with the file:
// a field changes meaning, or the folder layout changes. Read refuses a
// file newer than this package, and Update never writes over one, so an
// old admin can't undo what a newer agent recorded. Update keeps fields it
// doesn't know, so an older writer doesn't drop what a newer one added at
// the same version.
//
// # Writers
//
// mc-agent and admin may both write: Update holds an exclusive flock on
// <apps>/state.lock around its read, change and write. Writes go to a
// temporary file that is synced and renamed over state.json, so readers
// never take the lock and never see half a file.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Version is the newest state.json format this package reads and writes.
const Version = 1

// File names in the apps folder.
const (
	FileName = "state.json"
	lockName = "state.lock"
)

// Layout is how releases are arranged in the apps folder.
type Layout string

const (
	// Single is one release for the whole Pi in <apps>/current. A missing
	// state.json means Single.
	Single Layout = "single"
	// Tracks is one folder per track, <apps>/<track>, each with its own
	// current release.
	Tracks Layout = "tracks"
)

// State is <apps>/state.json.
type State struct {
	// Version is the format version; see the package doc.
	Version int `json:"version"`
	// Layout says where releases are. A missing state.json is Single.
	Layout Layout `json:"layout"`
	// Tracks lists the track folders in use, in the order mc-agent deploys
	// them. Empty for Single.
	Tracks []string `json:"tracks,omitzero"`
	// Migration records the last move from Single to Tracks that failed,
	// so mc-agent doesn't retry it until a track has a new release. Nil
	// once the move worked, or if it never failed.
	Migration *Migration `json:"migration,omitzero"`

	// extra holds fields this version doesn't know, written back as-is.
	extra map[string]json.RawMessage
}

// Migration is an attempt to move from Single to Tracks that failed.
type Migration struct {
	Tags  map[string]string `json:"tags"` // the release it tried, per track
	Error string            `json:"error"`
	At    time.Time         `json:"at"`
}

// TrackDir returns the folder of a track in the Tracks layout.
func TrackDir(apps, track string) string { return filepath.Join(apps, track) }

// ErrUnsupported is returned (wrapped) for a state.json newer than Version.
var ErrUnsupported = errors.New("unsupported state version")

// Read returns the state in apps. A missing file is the Single layout at
// version 0. A newer version than this package knows is an error wrapping
// ErrUnsupported.
func Read(apps string) (*State, error) {
	data, err := os.ReadFile(filepath.Join(apps, FileName))
	if errors.Is(err, fs.ErrNotExist) {
		return &State{Layout: Single}, nil
	}
	if err != nil {
		return nil, err
	}
	return decode(data)
}

func decode(data []byte) (*State, error) {
	var head struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("state: reading %s: %w", FileName, err)
	}
	if head.Version > Version {
		return nil, fmt.Errorf("state: %w %d (this program knows up to %d); update it", ErrUnsupported, head.Version, Version)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("state: reading %s: %w", FileName, err)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, fmt.Errorf("state: reading %s: %w", FileName, err)
	}
	for _, known := range []string{"version", "layout", "tracks", "migration"} {
		delete(all, known)
	}
	if len(all) > 0 {
		s.extra = all
	}
	if s.Layout == "" {
		s.Layout = Single
	}
	return &s, nil
}

// MarshalJSON writes the known fields plus any this version didn't know.
func (s State) MarshalJSON() ([]byte, error) {
	type known State // without the method, so this doesn't recurse
	data, err := json.Marshal(known(s))
	if err != nil || len(s.extra) == 0 {
		return data, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err
	}
	merged := maps.Clone(s.extra)
	maps.Copy(merged, all)
	return json.Marshal(merged)
}

// Update reads the state in apps, lets change modify it, and writes it
// back at Version, all while holding <apps>/state.lock. If change returns
// an error, nothing is written. It fails without writing if the file is
// newer than Version.
func Update(apps string, change func(*State) error) error {
	unlock, err := lock(apps)
	if err != nil {
		return err
	}
	defer unlock()
	s, err := Read(apps)
	if err != nil {
		return err
	}
	if err := change(s); err != nil {
		return err
	}
	s.Version = Version
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(apps, FileName), append(data, '\n'))
}

// lock waits for an exclusive lock on <apps>/state.lock. Writers hold it
// only for a read and a write, so waiting is fine.
func lock(apps string) (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(apps, lockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

// writeFile replaces path atomically and durably.
func writeFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o644)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		dir.Sync()
		dir.Close()
	}
	return nil
}
