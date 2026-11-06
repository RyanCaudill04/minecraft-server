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

func utc(month time.Month, day, hour int) time.Time {
	return time.Date(2026, month, day, hour, 0, 0, 0, time.UTC)
}

func earned(id, name, adv, title string, at time.Time) world.Event {
	return world.Event{Kind: world.EventAdvancement, At: at, PlayerID: id, Name: name, Advancement: adv, Title: title}
}

func joined(id, name string, at time.Time) world.Event {
	return world.Event{Kind: world.EventJoined, At: at, PlayerID: id, Name: name}
}

// fixtureActivity is every event in worldtest's world, newest first.
func fixtureActivity() []world.Event {
	alice, bob := worldtest.AliceID, worldtest.BobID
	return []world.Event{
		// The same instant: the lower ID goes first.
		earned(alice, "Alice", "minecraft:end/elytra", "Sky's the Limit", utc(10, 20, 12)),
		earned(bob, "Bob", "minecraft:end/elytra", "Sky's the Limit", utc(10, 20, 12)),
		// Done when its last criterion was.
		earned(bob, "Bob", "minecraft:story/enter_the_end", "The End?", utc(10, 9, 12)),
		earned(alice, "Alice", "minecraft:story/enter_the_end", "The End?", utc(10, 8, 12)),
		earned(bob, "Bob", "minecraft:story/mine_diamond", "Diamonds!", utc(10, 2, 10)),
		// A join at the same instant as an advancement goes below it.
		earned(bob, "Bob", "minecraft:story/enter_the_nether", "We Need to Go Deeper", utc(10, 2, 9)),
		joined(bob, "Bob", utc(10, 2, 9)),
		// Written as 2026-10-01 20:00:00 -0500.
		earned(alice, "Alice", "minecraft:story/enter_the_nether", "We Need to Go Deeper", utc(10, 2, 1)),
		// From an unfinished advancement's criterion, before anything done.
		joined(alice, "Alice", utc(10, 1, 0)),
	}
}

func TestReadActivity(t *testing.T) {
	server := worldtest.Build(t)

	got, err := world.ReadActivity(server, filepath.Join(server, "world"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := fixtureActivity(); !reflect.DeepEqual(got, want) {
		t.Errorf("ReadActivity:\n got %+v\nwant %+v", got, want)
	}
}

func TestReadActivityLimit(t *testing.T) {
	server := worldtest.Build(t)
	all := fixtureActivity()

	for _, limit := range []int{1, 3, len(all), len(all) + 5, 0, -1} {
		got, err := world.ReadActivity(server, filepath.Join(server, "world"), limit)
		if err != nil {
			t.Fatal(err)
		}
		want := all
		if limit > 0 && limit < len(all) {
			want = all[:limit]
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("limit %d:\n got %+v\nwant %+v", limit, got, want)
		}
	}
}

func TestReadActivityFilters(t *testing.T) {
	server := worldtest.Build(t)
	if err := os.Remove(filepath.Join(server, "world", "players", "advancements", worldtest.BobID+".json")); err != nil {
		t.Fatal(err)
	}
	worldtest.WriteAdvancements(t, server, worldtest.AliceID, map[string]any{
		// In the feed, with times in different zones. All three are
		// 2026-10-05 03:00 UTC, so they tie and sort by ID.
		"minecraft:story/mine_diamond":   worldtest.Advancement("2026-10-05 12:00:00 +0900"),
		"minecraft:story/smelt_iron":     worldtest.Advancement("2026-10-04 22:00:00 -0500"),
		"minecraft:story/mine_stone":     worldtest.Advancement("2026-10-05 03:00:00 +0000"),
		"minecraft:story/upgrade_tools":  worldtest.Advancement("2026-10-05 04:00:00 +0000", "2026-10-06 04:00:00 +0000"),
		"minecraft:story/follow_unknown": worldtest.Advancement("2026-10-07 04:00:00 +0000"),
		// Never in the feed.
		"minecraft:recipes/misc/torch":       worldtest.Advancement("2026-10-08 00:00:00 +0000"),
		"minecraft:story/root":               worldtest.Advancement("2026-10-08 00:00:00 +0000"),
		"someplugin:hidden/bookkeeping":      worldtest.Advancement("2026-10-08 00:00:00 +0000"),
		"minecraft:end/kill_dragon":          map[string]any{"done": false, "criteria": map[string]any{"killed_dragon": "2026-10-08 00:00:00 +0000"}},
		"minecraft:nether/summon_wither":     worldtest.Advancement("2026-10-08 00:00"), // no zone
		"minecraft:adventure/kill_a_mob":     map[string]any{"done": true},
		"minecraft:adventure/sleep_in_bed":   "not an object",
		"minecraft:adventure/adventuring_ti": map[string]any{"done": true, "criteria": []string{"a list"}},
		// The earliest time anywhere in the file is the join, even from a
		// recipe.
		"minecraft:recipes/misc/stick": worldtest.Advancement("2026-09-30 23:30:00 -0100"),
		"DataVersion":                  4903,
	})

	got, err := world.ReadActivity(server, filepath.Join(server, "world"), 0)
	if err != nil {
		t.Fatal(err)
	}
	id := worldtest.AliceID
	want := []world.Event{
		earned(id, "Alice", "minecraft:story/follow_unknown", "Follow unknown", utc(10, 7, 4)),
		earned(id, "Alice", "minecraft:story/upgrade_tools", "Getting an Upgrade", utc(10, 6, 4)),
		earned(id, "Alice", "minecraft:story/mine_diamond", "Diamonds!", utc(10, 5, 3)),
		earned(id, "Alice", "minecraft:story/mine_stone", "Stone Age", utc(10, 5, 3)),
		earned(id, "Alice", "minecraft:story/smelt_iron", "Acquire Hardware", utc(10, 5, 3)),
		joined(id, "Alice", time.Date(2026, 10, 1, 0, 30, 0, 0, time.UTC)),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadActivity:\n got %+v\nwant %+v", got, want)
	}
}

// Files are rewritten in place while the server runs, so a read can catch
// one empty or half written. That must only cost that player's events, not
// the whole feed (see #38).
func TestReadActivityBadFiles(t *testing.T) {
	aliceFile := filepath.Join("world", "players", "advancements", worldtest.AliceID+".json")
	bobOnly := func() []world.Event {
		var events []world.Event
		for _, e := range fixtureActivity() {
			if e.PlayerID == worldtest.BobID {
				events = append(events, e)
			}
		}
		return events
	}

	for name, content := range map[string]string{
		"empty":         "",
		"half written":  `{"minecraft:story/enter_the_nether": {"criteria": {"entered": "2026-10-0`,
		"not an object": `["minecraft:story/enter_the_nether"]`,
		"null":          "null",
	} {
		t.Run(name, func(t *testing.T) {
			server := worldtest.Build(t)
			if err := os.WriteFile(filepath.Join(server, aliceFile), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := world.ReadActivity(server, filepath.Join(server, "world"), 0)
			if err != nil {
				t.Fatalf("one bad file failed the feed: %v", err)
			}
			if want := bobOnly(); !reflect.DeepEqual(got, want) {
				t.Errorf("ReadActivity:\n got %+v\nwant %+v", got, want)
			}
		})
	}

	t.Run("unreadable file", func(t *testing.T) {
		server := worldtest.Build(t)
		path := filepath.Join(server, aliceFile)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o755); err != nil { // reading a folder fails
			t.Fatal(err)
		}
		got, err := world.ReadActivity(server, filepath.Join(server, "world"), 0)
		if err != nil {
			t.Fatal(err)
		}
		if want := bobOnly(); !reflect.DeepEqual(got, want) {
			t.Errorf("ReadActivity:\n got %+v\nwant %+v", got, want)
		}
	})

	t.Run("half-written usercache.json", func(t *testing.T) {
		server := worldtest.Build(t)
		if err := os.WriteFile(filepath.Join(server, "usercache.json"), []byte(`[{"uuid": "1111`), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := world.ReadActivity(server, filepath.Join(server, "world"), 0)
		if err != nil {
			t.Fatal(err)
		}
		want := fixtureActivity()
		for i := range want {
			want[i].Name = want[i].PlayerID[:8]
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ReadActivity:\n got %+v\nwant %+v", got, want)
		}
	})
}

func TestReadActivityEmptyWorld(t *testing.T) {
	server := t.TempDir()
	got, err := world.ReadActivity(server, filepath.Join(server, "world"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("got %#v, want an empty, non-nil slice (it encodes as [] not null)", got)
	}
}

func TestAdvancementTitle(t *testing.T) {
	for id, want := range map[string]string{
		"minecraft:story/mine_diamond":           "Diamonds!",
		"minecraft:nether/charge_respawn_anchor": `Not Quite "Nine" Lives`,
		"minecraft:story/mine_copper":            "Mine copper",
		"minecraft:adventure/brand_new_thing":    "Brand new thing",
		"someplugin:quests/first_quest":          "First quest",
		"no_namespace":                           "No namespace",
		"minecraft:story/":                       "minecraft:story/",
		"":                                       "",
	} {
		if got := world.AdvancementTitle(id); got != want {
			t.Errorf("AdvancementTitle(%q) = %q, want %q", id, got, want)
		}
	}
}
