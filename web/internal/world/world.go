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
	"log"
	"os"
	"path/filepath"
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
	LastSaved  time.Time `json:"lastSaved"`
	Spawn      []int64   `json:"spawn,omitempty"` // x, y, z
}

// Player is one player's saved statistics.
type Player struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	LastSeen     time.Time `json:"lastSeen"`
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
}

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

	// The day/night clock and weather are per-dimension files. They are
	// optional: a fresh world may not have written them yet. One that can't
	// be read (say, caught mid-save) is treated as missing, so a cosmetic
	// field doesn't take the whole summary down.
	data := filepath.Join(worldDir, filepath.FromSlash(dataDir))
	if clocks := readOptionalNBT(filepath.Join(data, "world_clocks.dat")); clocks != nil {
		ticks, _ := nbt.Int(clocks, "data", "minecraft:overworld", "total_ticks")
		info.Day = ticks/TicksPerDay + 1
		info.TimeOfDay = ticks % TicksPerDay
	}
	if weather := readOptionalNBT(filepath.Join(data, "weather.dat")); weather != nil {
		raining, _ := nbt.Int(weather, "data", "raining")
		thundering, _ := nbt.Int(weather, "data", "thundering")
		info.Raining = raining != 0
		info.Thundering = thundering != 0
	}
	return info, nil
}

// dataDir is the overworld's data folder, relative to the world folder, in
// slash form. It holds the clock and weather files.
const dataDir = "dimensions/minecraft/overworld/data/minecraft"

// readOptionalNBT reads an NBT file, returning nil if it is missing or
// unreadable. Unreadable files are logged.
func readOptionalNBT(path string) map[string]any {
	m, err := nbt.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("world: skipping %v", err)
		}
		return nil
	}
	return m
}

// ReadPlayers reads every player with a stats file in worldDir, newest
// LastSeen first. Names come from usercache.json in serverDir; players
// missing from it are named by the start of their UUID.
//
// The server rewrites these files in place, so a read can land on one that
// is empty or half-written. A bad file only affects what it holds, and is
// logged: a bad usercache.json means UUID names, a bad stats file skips that
// player, and a bad advancements file counts as 0.
func ReadPlayers(serverDir, worldDir string) ([]Player, error) {
	names, err := readUserCache(filepath.Join(serverDir, "usercache.json"))
	if err != nil {
		log.Printf("world: skipping %v", err)
		names = map[string]string{}
	}

	playersDir := filepath.Join(worldDir, "players")
	statFiles, err := filepath.Glob(filepath.Join(playersDir, "stats", "*.json"))
	if err != nil {
		return nil, fmt.Errorf("world: %w", err)
	}

	players := make([]Player, 0, len(statFiles))
	for _, path := range statFiles {
		id := strings.TrimSuffix(filepath.Base(path), ".json")
		p := Player{ID: id, Name: names[id]}
		if p.Name == "" {
			p.Name = id[:min(8, len(id))]
		}
		if err := readStats(path, &p); err != nil {
			log.Printf("world: skipping player: stats/%s.json: %v", id, err)
			continue
		}
		// The player data file is rewritten on every save while they're
		// online and on logout, so its time is when they were last seen.
		if fi, err := os.Stat(filepath.Join(playersDir, "data", id+".dat")); err == nil {
			p.LastSeen = fi.ModTime().UTC()
		}
		p.Advancements, err = countAdvancements(filepath.Join(playersDir, "advancements", id+".json"))
		if err != nil {
			log.Printf("world: advancements/%s.json: %v", id, err)
		}
		players = append(players, p)
	}

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

// countAdvancements counts completed advancements, skipping recipe unlocks
// (which the game also tracks as advancements). A missing file counts as 0.
func countAdvancements(path string) (int, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	// Values are objects, except DataVersion, which is a number.
	var file map[string]json.RawMessage
	if err := json.Unmarshal(data, &file); err != nil {
		return 0, err
	}
	n := 0
	for key, raw := range file {
		if strings.Contains(key, ":recipes/") {
			continue
		}
		var adv struct {
			Done bool `json:"done"`
		}
		if json.Unmarshal(raw, &adv) == nil && adv.Done {
			n++
		}
	}
	return n, nil
}
