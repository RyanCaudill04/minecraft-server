// Package listing decides which players play may show by name.
//
// Minecraft has a per-player setting, "Allow Server Listings". With it on,
// the server's status ping lists the player by name and UUID; with it off,
// Paper sends "Anonymous Player" with an all-zero UUID instead. That is the
// only consent signal play has, so a Registry learns who is public from
// status pings and nothing else.
//
// The files on disk (stats, last-seen times) don't know about the setting.
// A player who was listed last week and hides today would still have their
// new playtime and last-seen time written to disk. So a player is public
// only while their data is no newer than the last time the ping listed them
// (plus Grace, for the save Paper makes as they log out). Once they play
// without being listed, they drop out until they are listed again.
// Anything unknown counts as hidden.
package listing

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/mcstatus"
)

// DefaultGrace covers the save Paper makes when a listed player logs out,
// which lands after the last ping that listed them. play pings every 15s.
const DefaultGrace = 30 * time.Second

// MaxPlayers caps how many players a Registry remembers.
const MaxPlayers = 1024

// saveEvery limits how often a still-listed player's time is written to
// disk. A new player is always written at once. After a restart, a stale
// time can only hide someone until they're listed again, never show them.
const saveEvery = time.Minute

var (
	uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	namePattern = regexp.MustCompile(`^[A-Za-z0-9_.]{1,16}$`)
)

// Entry is a player the server list has shown by name.
type Entry struct {
	Name   string    `json:"name"`
	Listed time.Time `json:"listed"` // last ping that listed them
}

// Registry remembers when each player was last listed by name. It is safe
// for concurrent use.
type Registry struct {
	path  string
	grace time.Duration

	mu    sync.Mutex
	seen  map[string]Entry // by UUID
	saved map[string]time.Time
}

// Open returns a Registry kept in path, reading what an earlier run saved.
// An empty path keeps it in memory only. A missing file is an empty
// registry. An unreadable or damaged file is an error, but the Registry is
// still usable and starts empty: everyone is hidden until listed again,
// and the next save replaces the file.
func Open(path string, grace time.Duration) (*Registry, error) {
	r := &Registry{path: path, grace: grace, seen: map[string]Entry{}, saved: map[string]time.Time{}}
	if path == "" {
		return r, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, fmt.Errorf("listing: %w", err)
	}
	var file map[string]Entry
	if err := json.Unmarshal(data, &file); err != nil {
		return r, fmt.Errorf("listing: %s: %w", path, err)
	}
	for id, e := range file {
		if valid(id, e.Name) && len(r.seen) < MaxPlayers {
			r.seen[id] = e
			r.saved[id] = e.Listed
		}
	}
	return r, nil
}

// Observe records the players a status ping from Paper listed by name at
// time at. Anonymous and malformed entries are ignored. Only pings from
// Paper itself count: LazyMC's sample isn't the real player list.
func (r *Registry) Observe(players []mcstatus.Player, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	dirty := false
	for _, p := range players {
		if !valid(p.ID, p.Name) {
			continue
		}
		old, known := r.seen[p.ID]
		if !known && len(r.seen) >= MaxPlayers {
			continue
		}
		if known && !at.After(old.Listed) && old.Name == p.Name {
			continue
		}
		r.seen[p.ID] = Entry{Name: p.Name, Listed: at}
		if !known || old.Name != p.Name || at.Sub(r.saved[p.ID]) >= saveEvery {
			dirty = true
		}
	}
	if !dirty || r.path == "" {
		return nil
	}
	if err := r.save(); err != nil {
		return err
	}
	for id, e := range r.seen {
		r.saved[id] = e.Listed
	}
	return nil
}

// Public reports whether the player with this UUID may be shown by name,
// given the last time their data changed on disk (zero if unknown). They
// must have been listed by name, and not have played since without being
// listed.
func (r *Registry) Public(id string, lastSeen time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.seen[id]
	if !ok {
		return false
	}
	return !lastSeen.After(e.Listed.Add(r.grace))
}

// save writes the registry, replacing the file atomically. The caller
// holds r.mu.
func (r *Registry) save() error {
	data, err := json.Marshal(r.seen)
	if err != nil {
		return fmt.Errorf("listing: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(r.path), ".listed-*")
	if err != nil {
		return fmt.Errorf("listing: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("listing: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("listing: %w", err)
	}
	if err := os.Rename(tmp.Name(), r.path); err != nil {
		return fmt.Errorf("listing: %w", err)
	}
	return nil
}

// valid reports whether id and name look like a real listed player: a
// lowercase hyphenated UUID that isn't the anonymous one, and a Minecraft
// name (Floodgate prefixes Bedrock names with a dot).
func valid(id, name string) bool {
	return id != mcstatus.AnonymousID && uuidPattern.MatchString(id) && namePattern.MatchString(name)
}
