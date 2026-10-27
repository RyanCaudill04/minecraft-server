package world_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/world"
	"github.com/ryancaudill04/minecraft-server/web/internal/world/worldtest"
)

func TestReadInfo(t *testing.T) {
	server := worldtest.Build(t)

	got, err := world.ReadInfo(filepath.Join(server, "world"))
	if err != nil {
		t.Fatal(err)
	}
	want := &world.Info{
		Name:       "world",
		Version:    "26.2",
		Difficulty: "easy",
		AgeTicks:   worldtest.AgeTicks,
		Day:        315,
		TimeOfDay:  5453,
		Raining:    true,
		LastSaved:  time.UnixMilli(worldtest.LastSaved).UTC(),
		Spawn:      []int64{192, 76, -192},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadInfo:\n got %+v\nwant %+v", got, want)
	}
}

func TestReadInfoOptionalFiles(t *testing.T) {
	server := worldtest.Build(t)
	data := filepath.Join(server, "world", "dimensions", "minecraft", "overworld", "data", "minecraft")
	for _, name := range []string{"world_clocks.dat", "weather.dat"} {
		if err := os.Remove(filepath.Join(data, name)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := world.ReadInfo(filepath.Join(server, "world"))
	if err != nil {
		t.Fatalf("a world without clock or weather files: %v", err)
	}
	if got.Day != 0 || got.Raining {
		t.Errorf("Day = %d, Raining = %v; want zero values", got.Day, got.Raining)
	}
}

func TestReadInfoErrors(t *testing.T) {
	t.Run("missing level.dat", func(t *testing.T) {
		if _, err := world.ReadInfo(t.TempDir()); err == nil {
			t.Error("want an error")
		}
	})
	t.Run("corrupt clock file", func(t *testing.T) {
		server := worldtest.Build(t)
		path := filepath.Join(server, "world", "dimensions", "minecraft", "overworld", "data", "minecraft", "world_clocks.dat")
		if err := os.WriteFile(path, []byte("not nbt"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := world.ReadInfo(filepath.Join(server, "world")); err == nil {
			t.Error("want an error")
		}
	})
}

func TestReadPlayers(t *testing.T) {
	server := worldtest.Build(t)

	got, err := world.ReadPlayers(server, filepath.Join(server, "world"))
	if err != nil {
		t.Fatal(err)
	}
	want := []world.Player{
		{
			ID:        worldtest.BobID,
			Name:      "Bob",
			LastSeen:  worldtest.BobSeen,
			PlayTicks: 36000,
			MobKills:  5,
		},
		{
			ID:           worldtest.AliceID,
			Name:         "Alice",
			LastSeen:     worldtest.AliceSeen,
			PlayTicks:    72000,
			Deaths:       2,
			MobKills:     40,
			BlocksMined:  123,
			DistanceCM:   75000,
			Jumps:        300,
			Advancements: 2, // recipes and unfinished ones don't count
		},
		{
			ID:   worldtest.CarolID,
			Name: "33333333", // not in usercache.json; never seen
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadPlayers:\n got %+v\nwant %+v", got, want)
	}
}

func TestReadPlayersWithoutUserCache(t *testing.T) {
	server := worldtest.Build(t)
	if err := os.Remove(filepath.Join(server, "usercache.json")); err != nil {
		t.Fatal(err)
	}

	got, err := world.ReadPlayers(server, filepath.Join(server, "world"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got {
		if p.Name != p.ID[:8] {
			t.Errorf("player %s named %q, want the UUID prefix", p.ID, p.Name)
		}
	}
}

func TestReadPlayersErrors(t *testing.T) {
	corrupt := map[string]string{
		"usercache.json":    "usercache.json",
		"stats file":        filepath.Join("world", "players", "stats", worldtest.AliceID+".json"),
		"advancements file": filepath.Join("world", "players", "advancements", worldtest.AliceID+".json"),
	}
	for name, rel := range corrupt {
		t.Run(name, func(t *testing.T) {
			server := worldtest.Build(t)
			if err := os.WriteFile(filepath.Join(server, rel), []byte("{not json"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := world.ReadPlayers(server, filepath.Join(server, "world")); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestReadPlayersEmptyWorld(t *testing.T) {
	server := t.TempDir()
	got, err := world.ReadPlayers(server, filepath.Join(server, "world"))
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("got %#v, want an empty, non-nil slice (it encodes as [] not null)", got)
	}
}
