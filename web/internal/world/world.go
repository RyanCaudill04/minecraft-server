// Package world reads facts and player statistics from a Minecraft server's
// files on disk. Everything comes from saved files, so it works while the
// server is asleep, but values only change when the server saves (on
// autosave and on shutdown).
//
// Paths follow the Minecraft 26.x layout, where per-player files live under
// world/players and per-dimension data under world/dimensions.
package world

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/nbt"
)

// TicksPerDay is the length of one Minecraft day. Tick 0 is sunrise (06:00).
const TicksPerDay = 24000

// Info is a summary of the world from its saved files.
type Info struct {
	Name       string    `json:"name"`
	Version    string    `json:"version"`
	Difficulty string    `json:"difficulty"`
	Hardcore   bool      `json:"hardcore"`
	AgeTicks   int64     `json:"ageTicks"`  // ticks the world has run
	Day        int64     `json:"day"`       // in-game day number, from 1
	TimeOfDay  int64     `json:"timeOfDay"` // 0-23999, 0 is sunrise
	Raining    bool      `json:"raining"`
	Thundering bool      `json:"thundering"`
	LastSaved  time.Time `json:"lastSaved,omitzero"` // left out when unknown
	Spawn      []int64   `json:"spawn,omitempty"`    // x, y, z
	Players    int       `json:"players"`            // how many have joined, hidden ones included
}

// Player is one player's saved statistics.
type Player struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	LastSeen     time.Time `json:"lastSeen,omitzero"` // left out when unknown
	PlayTicks    int64     `json:"playTicks"`
	Deaths       int64     `json:"deaths"`
	MobKills     int64     `json:"mobKills"`
	PlayerKills  int64     `json:"playerKills"`
	BlocksMined  int64     `json:"blocksMined"`
	DistanceCM   int64     `json:"distanceCm"` // all *_one_cm movement stats
	Jumps        int64     `json:"jumps"`
	FishCaught   int64     `json:"fishCaught"`
	AnimalsBred  int64     `json:"animalsBred"`
	Advancements int       `json:"advancements"` // completed, not counting recipes
	Firsts       []First   `json:"firsts,omitempty"`
}

// Firsts are the milestones play shows as badges, in display order. Whoever
// completed one earliest holds its badge.
var Firsts = []string{
	"minecraft:story/enter_the_nether",
	"minecraft:story/mine_diamond",
	"minecraft:story/enter_the_end",
	"minecraft:end/kill_dragon",
	"minecraft:nether/summon_wither",
	"minecraft:end/elytra",
}

// First is a milestone from Firsts that a player reached before anyone else.
type First struct {
	Advancement string    `json:"advancement"`
	At          time.Time `json:"at"`
}

// advancementTime is how advancement files write criteria times.
const advancementTime = "2006-01-02 15:04:05 -0700"

// ReadInfo reads the world summary from worldDir (e.g. ~/minecraft_server/world).
func ReadInfo(worldDir string) (*Info, error) {
	levelPath := filepath.Join(worldDir, "level.dat")
	level, err := nbt.ReadFile(levelPath)
	if err != nil {
		return nil, fmt.Errorf("world: %w", err)
	}

	info := &Info{}
	info.Name, _ = nbt.String(level, "Data", "LevelName")
	info.Version, _ = nbt.String(level, "Data", "Version", "Name")
	info.Difficulty, _ = nbt.String(level, "Data", "difficulty_settings", "difficulty")
	hardcore, _ := nbt.Int(level, "Data", "difficulty_settings", "hardcore")
	info.Hardcore = hardcore != 0
	info.AgeTicks, _ = nbt.Int(level, "Data", "Time")
	if ms, ok := nbt.Int(level, "Data", "LastPlayed"); ok {
		info.LastSaved = time.UnixMilli(ms).UTC()
	}
	if pos, ok := nbt.Get(level, "Data", "spawn", "pos").([]int32); ok && len(pos) == 3 {
		info.Spawn = []int64{int64(pos[0]), int64(pos[1]), int64(pos[2])}
	}
	// A count only: who they are is up to package listing.
	statFiles, err := filepath.Glob(filepath.Join(worldDir, "players", "stats", "*.json"))
	if err != nil {
		return nil, fmt.Errorf("world: %w", err)
	}
	info.Players = len(statFiles)

	// The day/night clock and weather are per-dimension files. They are
	// optional: a fresh world may not have written them yet.
	data := filepath.Join(worldDir, "dimensions", "minecraft", "overworld", "data", "minecraft")
	if clocks, err := nbt.ReadFile(filepath.Join(data, "world_clocks.dat")); err == nil {
		ticks, _ := nbt.Int(clocks, "data", "minecraft:overworld", "total_ticks")
		info.Day = ticks/TicksPerDay + 1
		info.TimeOfDay = ticks % TicksPerDay
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("world: %w", err)
	}
	if weather, err := nbt.ReadFile(filepath.Join(data, "weather.dat")); err == nil {
		raining, _ := nbt.Int(weather, "data", "raining")
		thundering, _ := nbt.Int(weather, "data", "thundering")
		info.Raining = raining != 0
		info.Thundering = thundering != 0
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("world: %w", err)
	}
	return info, nil
}

// ReadPlayers reads every player with a stats file in worldDir, newest
// LastSeen first. Names come from usercache.json in serverDir; players
// missing from it are named by the start of their UUID.
func ReadPlayers(serverDir, worldDir string) ([]Player, error) {
	names, err := readUserCache(filepath.Join(serverDir, "usercache.json"))
	if err != nil {
		return nil, fmt.Errorf("world: %w", err)
	}

	playersDir := filepath.Join(worldDir, "players")
	statFiles, err := filepath.Glob(filepath.Join(playersDir, "stats", "*.json"))
	if err != nil {
		return nil, fmt.Errorf("world: %w", err)
	}

	players := make([]Player, 0, len(statFiles))
	earned := make(map[string]map[string]time.Time, len(statFiles)) // ID -> advancement -> when
	for _, path := range statFiles {
		id := strings.TrimSuffix(filepath.Base(path), ".json")
		p := Player{ID: id, Name: names[id]}
		if p.Name == "" {
			p.Name = id[:min(8, len(id))]
		}
		if err := readStats(path, &p); err != nil {
			return nil, fmt.Errorf("world: %s: %w", id, err)
		}
		p.Advancements, earned[id], err = readAdvancements(filepath.Join(playersDir, "advancements", id+".json"))
		if err != nil {
			return nil, fmt.Errorf("world: %s: %w", id, err)
		}
		// The player data file is rewritten on every save while they're
		// online and on logout, and Paper saves the stats and advancements
		// with it. LastSeen is the newest of the three, taken after the
		// contents were read, so a save that lands mid-read looks newer
		// than what was read rather than older.
		for _, f := range []string{
			filepath.Join(playersDir, "data", id+".dat"),
			path,
			filepath.Join(playersDir, "advancements", id+".json"),
		} {
			if fi, err := os.Stat(f); err == nil && fi.ModTime().After(p.LastSeen) {
				p.LastSeen = fi.ModTime().UTC()
			}
		}
		players = append(players, p)
	}
	markFirsts(players, earned)

	sort.Slice(players, func(i, j int) bool { return players[i].LastSeen.After(players[j].LastSeen) })
	return players, nil
}

// readUserCache maps UUIDs to names using the server's usercache.json.
func readUserCache(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("usercache.json: %w", err)
	}
	names := make(map[string]string, len(entries))
	for _, e := range entries {
		names[e.UUID] = e.Name
	}
	return names, nil
}

// readStats fills p's statistics from a stats/<uuid>.json file.
func readStats(path string, p *Player) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var file struct {
		Stats map[string]map[string]int64 `json:"stats"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return err
	}

	custom := file.Stats["minecraft:custom"]
	p.PlayTicks = custom["minecraft:play_time"]
	p.Deaths = custom["minecraft:deaths"]
	p.MobKills = custom["minecraft:mob_kills"]
	p.PlayerKills = custom["minecraft:player_kills"]
	p.Jumps = custom["minecraft:jump"]
	p.FishCaught = custom["minecraft:fish_caught"]
	p.AnimalsBred = custom["minecraft:animals_bred"]
	for key, v := range custom {
		if strings.HasSuffix(key, "_one_cm") {
			p.DistanceCM += v
		}
	}
	for _, v := range file.Stats["minecraft:mined"] {
		p.BlocksMined += v
	}
	return nil
}

// readAdvancements counts completed advancements, skipping recipe unlocks
// (which the game also tracks as advancements), and returns when each
// completed one in Firsts was done: the time of its last criterion. A done
// advancement without a readable time is counted but has no time. A missing
// file counts as 0.
func readAdvancements(path string) (int, map[string]time.Time, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil, nil
	}
	if err != nil {
		return 0, nil, err
	}
	// Values are objects, except DataVersion, which is a number.
	var file map[string]json.RawMessage
	if err := json.Unmarshal(data, &file); err != nil {
		return 0, nil, err
	}
	n := 0
	done := map[string]time.Time{}
	for key, raw := range file {
		if strings.Contains(key, ":recipes/") {
			continue
		}
		// Criteria are read loosely, so an odd one costs only the time.
		var adv struct {
			Done     bool           `json:"done"`
			Criteria map[string]any `json:"criteria"`
		}
		if json.Unmarshal(raw, &adv) != nil || !adv.Done {
			continue
		}
		n++
		if !slices.Contains(Firsts, key) {
			continue
		}
		var last time.Time
		for _, v := range adv.Criteria {
			s, _ := v.(string)
			if t, err := time.Parse(advancementTime, s); err == nil && t.After(last) {
				last = t
			}
		}
		if !last.IsZero() {
			done[key] = last.UTC()
		}
	}
	return n, done, nil
}

// markFirsts sets each player's Firsts. earned maps a player's ID to when
// they completed each advancement in Firsts. For each advancement, the player
// who completed it earliest gets it; on a tie, the lower ID wins. A player's
// Firsts follow the order of the Firsts list.
func markFirsts(players []Player, earned map[string]map[string]time.Time) {
	for _, adv := range Firsts {
		winner := -1
		var best time.Time
		for i, p := range players {
			at, ok := earned[p.ID][adv]
			if !ok {
				continue
			}
			if winner < 0 || at.Before(best) || at.Equal(best) && p.ID < players[winner].ID {
				winner, best = i, at
			}
		}
		if winner >= 0 {
			players[winner].Firsts = append(players[winner].Firsts, First{Advancement: adv, At: best})
		}
	}
}
