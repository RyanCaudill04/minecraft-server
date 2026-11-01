package world_test

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

// quietLog hides the package's log output for the rest of the test.
func quietLog(t *testing.T) {
	w := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(w) })
}

// A missing, empty or corrupt clock or weather file leaves its fields at
// zero, but the rest of the summary is still read.
func TestReadInfoOptionalFiles(t *testing.T) {
	quietLog(t)
	for name, change := range map[string]func(path string) error{
		"missing": os.Remove,
		"empty":   func(path string) error { return os.WriteFile(path, nil, 0o644) },
		"corrupt": func(path string) error { return os.WriteFile(path, []byte("not nbt"), 0o644) },
	} {
		t.Run(name, func(t *testing.T) {
			server := worldtest.Build(t)
			for _, file := range []string{"world_clocks.dat", "weather.dat"} {
				if err := change(filepath.Join(worldtest.DataDir(server), file)); err != nil {
					t.Fatal(err)
				}
			}

			got, err := world.ReadInfo(filepath.Join(server, "world"))
			if err != nil {
				t.Fatal(err)
			}
			if got.Day != 0 || got.TimeOfDay != 0 || got.Raining {
				t.Errorf("Day = %d, TimeOfDay = %d, Raining = %v; want zero values", got.Day, got.TimeOfDay, got.Raining)
			}
			if got.Name != "world" || got.AgeTicks != worldtest.AgeTicks {
				t.Errorf("Name = %q, AgeTicks = %d; want level.dat still read", got.Name, got.AgeTicks)
			}
		})
	}
}

func TestReadInfoErrors(t *testing.T) {
	t.Run("missing level.dat", func(t *testing.T) {
		if _, err := world.ReadInfo(t.TempDir()); err == nil {
			t.Error("want an error")
		}
	})
	t.Run("corrupt level.dat", func(t *testing.T) {
		server := worldtest.Build(t)
		path := filepath.Join(server, "world", "level.dat")
		if err := os.WriteFile(path, []byte("not nbt"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := world.ReadInfo(filepath.Join(server, "world"))
		if err == nil || !strings.Contains(err.Error(), "level.dat") {
			t.Errorf("error %v, want one naming level.dat", err)
		}
	})
}

// wantPlayers is what ReadPlayers returns for worldtest.Build.
func wantPlayers() []world.Player {
	return []world.Player{
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
}

func TestReadPlayers(t *testing.T) {
	server := worldtest.Build(t)

	got, err := world.ReadPlayers(server, filepath.Join(server, "world"))
	if err != nil {
		t.Fatal(err)
	}
	if want := wantPlayers(); !reflect.DeepEqual(got, want) {
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

// The server rewrites these files in place, so one can be empty or
// half-written when it's read. That must only affect what the file holds,
// not the whole list.
func TestReadPlayersBadFiles(t *testing.T) {
	quietLog(t)
	players := filepath.Join("world", "players")
	tests := []struct {
		name string
		rel  string // file to break, relative to the server folder
		want func([]world.Player) []world.Player
	}{
		{"usercache.json", "usercache.json", func(ps []world.Player) []world.Player {
			for i := range ps {
				ps[i].Name = ps[i].ID[:8]
			}
			return ps
		}},
		{"stats file", filepath.Join(players, "stats", worldtest.CarolID+".json"), func(ps []world.Player) []world.Player {
			return ps[:2] // Carol is skipped
		}},
		{"advancements file", filepath.Join(players, "advancements", worldtest.AliceID+".json"), func(ps []world.Player) []world.Player {
			ps[1].Advancements = 0
			return ps
		}},
	}
	for _, tt := range tests {
		for content, data := range map[string]string{"empty": "", "corrupt": "{not json"} {
			t.Run(tt.name+" "+content, func(t *testing.T) {
				server := worldtest.Build(t)
				if err := os.WriteFile(filepath.Join(server, tt.rel), []byte(data), 0o644); err != nil {
					t.Fatal(err)
				}
				got, err := world.ReadPlayers(server, filepath.Join(server, "world"))
				if err != nil {
					t.Fatal(err)
				}
				if want := tt.want(wantPlayers()); !reflect.DeepEqual(got, want) {
					t.Errorf("ReadPlayers:\n got %+v\nwant %+v", got, want)
				}
			})
		}
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
