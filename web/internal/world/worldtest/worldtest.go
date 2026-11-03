// Package worldtest builds a small synthetic Minecraft server folder for
// tests, in the 26.x layout package world reads. All players and stats are
// made up: the repo is public, so tests never use real player data.
package worldtest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/nbt/nbttest"
)

// Synthetic players. Carol has stats but isn't in usercache.json.
const (
	AliceID = "11111111-1111-1111-1111-111111111111"
	BobID   = "22222222-2222-2222-2222-222222222222"
	CarolID = "33333333-3333-3333-3333-333333333333"
)

// World facts written by Build.
const (
	TotalTicks = 7541453 // overworld clock: day 315, time 5453
	AgeTicks   = 7053076
	LastSaved  = 1793069177545 // Unix ms
)

// Last-seen times written by Build: Bob most recently, then Alice. Carol has
// no player data file.
var (
	AliceSeen = time.Date(2026, 10, 25, 20, 0, 0, 0, time.UTC)
	BobSeen   = time.Date(2026, 10, 26, 21, 0, 0, 0, time.UTC)
)

// DataDir returns the overworld data folder, holding the clock and weather
// files, of a server built by Build. It matches the path package world reads.
func DataDir(server string) string {
	return filepath.Join(server, "world", "dimensions", "minecraft", "overworld", "data", "minecraft")
}

// Build writes the synthetic server into a temporary folder and returns its
// path. The world is at <path>/world.
func Build(t testing.TB) string {
	t.Helper()
	server := t.TempDir()
	world := filepath.Join(server, "world")
	data := DataDir(server)
	players := filepath.Join(world, "players")

	nbttest.WriteFile(t, filepath.Join(world, "level.dat"), map[string]any{
		"Data": map[string]any{
			"LevelName":  "world",
			"Time":       int64(AgeTicks),
			"LastPlayed": int64(LastSaved),
			"Version":    map[string]any{"Name": "26.2", "Id": int32(4903)},
			"difficulty_settings": map[string]any{
				"difficulty": "easy",
				"hardcore":   int8(0),
				"locked":     int8(0),
			},
			"spawn": map[string]any{"pos": []int32{192, 76, -192}},
		},
	})
	nbttest.WriteFile(t, filepath.Join(data, "world_clocks.dat"), map[string]any{
		"data": map[string]any{
			"minecraft:overworld": map[string]any{"total_ticks": int64(TotalTicks)},
		},
	})
	nbttest.WriteFile(t, filepath.Join(data, "weather.dat"), map[string]any{
		"data": map[string]any{"raining": int8(1), "thundering": int8(0)},
	})

	writeJSON(t, filepath.Join(server, "usercache.json"), []map[string]string{
		{"uuid": AliceID, "name": "Alice"},
		{"uuid": BobID, "name": "Bob"},
	})

	writeJSON(t, filepath.Join(players, "stats", AliceID+".json"), stats(map[string]int64{
		"minecraft:play_time":   72000, // 1 hour
		"minecraft:deaths":      2,
		"minecraft:mob_kills":   40,
		"minecraft:walk_one_cm": 50000,
		"minecraft:fly_one_cm":  25000,
		"minecraft:jump":        300,
	}, map[string]int64{"minecraft:stone": 100, "minecraft:dirt": 23}))
	writeJSON(t, filepath.Join(players, "stats", BobID+".json"), stats(map[string]int64{
		"minecraft:play_time": 36000,
		"minecraft:mob_kills": 5,
	}, nil))
	writeJSON(t, filepath.Join(players, "stats", CarolID+".json"), stats(nil, nil))

	// Alice and Bob race for the milestones world.Firsts tracks. Alice wins
	// the Nether, the End and elytra; Bob wins diamonds.
	writeJSON(t, filepath.Join(players, "advancements", AliceID+".json"), map[string]any{
		"minecraft:story/root":             map[string]any{"done": true},
		"minecraft:story/mine_stone":       map[string]any{"done": true},
		"minecraft:story/smelt_iron":       map[string]any{"done": false},
		"minecraft:recipes/misc/torch":     map[string]any{"done": true},
		"minecraft:recipes/building/stone": map[string]any{"done": true},
		"minecraft:story/enter_the_nether": advancement("2026-10-01 20:00:00 -0500"),
		"minecraft:story/enter_the_end":    advancement("2026-10-08 12:00:00 +0000"),
		"minecraft:end/elytra":             advancement("2026-10-20 12:00:00 +0000"),
		// Unfinished, so no badge, and Bob's has no readable time.
		"minecraft:end/kill_dragon": map[string]any{"done": false, "criteria": map[string]any{"killed_dragon": "2026-10-01 00:00:00 +0000"}},
		"DataVersion":               4903,
	})
	writeJSON(t, filepath.Join(players, "advancements", BobID+".json"), map[string]any{
		"minecraft:story/enter_the_nether": advancement("2026-10-02 09:00:00 +0000"),
		"minecraft:story/mine_diamond":     advancement("2026-10-02 10:00:00 +0000"),
		// Done when the last criterion is, so after Alice.
		"minecraft:story/enter_the_end": advancement("2026-10-07 12:00:00 +0000", "2026-10-09 12:00:00 +0000"),
		// The same instant as Alice: the lower ID (Alice) wins the tie.
		"minecraft:end/elytra": advancement("2026-10-20 07:00:00 -0500"),
		// Done, but the time can't be read, so it can't be a first.
		"minecraft:nether/summon_wither": advancement("not a time"),
		// A criterion that isn't a time string still counts as done.
		"minecraft:end/kill_dragon": map[string]any{"done": true, "criteria": map[string]any{"killed_dragon": 5}},
		"DataVersion":               4903,
	})

	touch(t, filepath.Join(players, "data", AliceID+".dat"), AliceSeen)
	touch(t, filepath.Join(players, "data", BobID+".dat"), BobSeen)
	return server
}

func stats(custom, mined map[string]int64) map[string]any {
	s := map[string]any{}
	if custom != nil {
		s["minecraft:custom"] = custom
	}
	if mined != nil {
		s["minecraft:mined"] = mined
	}
	return map[string]any{"stats": s, "DataVersion": 4903}
}

// advancement is a completed advancement with one criterion per time, in
// the format advancement files use.
func advancement(times ...string) map[string]any {
	criteria := map[string]any{}
	for i, t := range times {
		criteria[fmt.Sprintf("criterion_%d", i)] = t
	}
	return map[string]any{"done": true, "criteria": criteria}
}

// writeFile writes data at path, creating parent folders.
func writeFile(t testing.TB, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeJSON(t testing.TB, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, data)
}

// touch creates an empty file with the given modification time.
func touch(t testing.TB, path string, mtime time.Time) {
	t.Helper()
	writeFile(t, path, nil)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}
