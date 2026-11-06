package world

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Kinds of activity event.
const (
	EventAdvancement = "advancement" // a player completed an advancement
	EventJoined      = "joined"      // a player's first recorded activity
)

// Event is one entry in the activity feed.
type Event struct {
	Kind        string    `json:"kind"`
	At          time.Time `json:"at"`
	PlayerID    string    `json:"playerId"`
	Name        string    `json:"name"`
	Advancement string    `json:"advancement,omitempty"` // only for EventAdvancement
	Title       string    `json:"title,omitempty"`       // the advancement's in-game title
}

// ReadActivity returns recent events across every player with an
// advancements file in worldDir, newest first, at most limit of them (no
// limit if limit <= 0). Names come from usercache.json in serverDir, as in
// ReadPlayers.
//
// An advancement's time is when its last criterion was met. Recipe unlocks,
// root advancements (which never show a toast) and advancements outside the
// minecraft namespace (often plugin bookkeeping without a display) are left
// out, as are advancements without a readable time.
//
// A player's join is the earliest criterion time anywhere in their file,
// recipes and unfinished advancements included: the game records some
// progress (such as the biome they spawn in) as soon as they first join.
//
// The server rewrites these files in place, so a read can catch one half
// written. A file that can't be read or parsed only loses that player's
// events, and an unreadable usercache.json only loses names.
func ReadActivity(serverDir, worldDir string, limit int) ([]Event, error) {
	names, err := readUserCache(filepath.Join(serverDir, "usercache.json"))
	if err != nil {
		names = map[string]string{}
	}
	files, err := filepath.Glob(filepath.Join(worldDir, "players", "advancements", "*.json"))
	if err != nil {
		return nil, fmt.Errorf("world: %w", err)
	}

	events := []Event{}
	for _, path := range files {
		id := strings.TrimSuffix(filepath.Base(path), ".json")
		name := names[id]
		if name == "" {
			name = id[:min(8, len(id))]
		}
		events = append(events, playerEvents(path, id, name)...)
	}

	sort.Slice(events, func(i, j int) bool {
		a, b := events[i], events[j]
		if !a.At.Equal(b.At) {
			return a.At.After(b.At)
		}
		// At the same instant, a join happened first, so it goes below.
		if a.Kind != b.Kind {
			return a.Kind != EventJoined
		}
		if a.PlayerID != b.PlayerID {
			return a.PlayerID < b.PlayerID
		}
		return a.Advancement < b.Advancement
	})
	if limit > 0 && len(events) > limit {
		events = events[:limit]
	}
	return events, nil
}

// playerEvents reads one player's advancements file. A file it can't read
// or parse has no events.
func playerEvents(path, id, name string) []Event {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var file map[string]json.RawMessage
	if json.Unmarshal(data, &file) != nil {
		return nil
	}

	var events []Event
	var first time.Time
	for key, raw := range file {
		var adv struct {
			Done     bool           `json:"done"`
			Criteria map[string]any `json:"criteria"`
		}
		// DataVersion is a number and fails here, as does anything odd.
		if json.Unmarshal(raw, &adv) != nil {
			continue
		}
		if t := earliestCriterion(adv.Criteria); !t.IsZero() && (first.IsZero() || t.Before(first)) {
			first = t
		}
		if !adv.Done || !inFeed(key) {
			continue
		}
		if at := lastCriterion(adv.Criteria); !at.IsZero() {
			events = append(events, Event{
				Kind:        EventAdvancement,
				At:          at,
				PlayerID:    id,
				Name:        name,
				Advancement: key,
				Title:       AdvancementTitle(key),
			})
		}
	}
	if !first.IsZero() {
		events = append(events, Event{Kind: EventJoined, At: first, PlayerID: id, Name: name})
	}
	return events
}

// inFeed reports whether a completed advancement belongs in the feed.
func inFeed(key string) bool {
	path, ok := strings.CutPrefix(key, "minecraft:")
	return ok && !strings.HasPrefix(path, "recipes/") && !strings.HasSuffix(path, "/root")
}

// lastCriterion is when the last of an advancement's criteria was met, in
// UTC, or the zero time if none has a readable time.
func lastCriterion(criteria map[string]any) time.Time {
	var last time.Time
	for _, t := range criteriaTimes(criteria) {
		if t.After(last) {
			last = t
		}
	}
	return last
}

// earliestCriterion is when the first of an advancement's criteria was met,
// in UTC, or the zero time if none has a readable time.
func earliestCriterion(criteria map[string]any) time.Time {
	var first time.Time
	for _, t := range criteriaTimes(criteria) {
		if first.IsZero() || t.Before(first) {
			first = t
		}
	}
	return first
}

// criteriaTimes parses every criterion time it can, in UTC. Values that
// aren't time strings are skipped.
func criteriaTimes(criteria map[string]any) []time.Time {
	times := make([]time.Time, 0, len(criteria))
	for _, v := range criteria {
		s, _ := v.(string)
		if t, err := time.Parse(advancementTime, s); err == nil {
			times = append(times, t.UTC())
		}
	}
	return times
}
