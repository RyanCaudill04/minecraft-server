package world

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/nbt/nbttest"
	"github.com/ryancaudill04/minecraft-server/web/internal/world/worldtest"
)

func TestReadSky(t *testing.T) {
	server := worldtest.Build(t)
	s, err := ReadSky(filepath.Join(server, "world"))
	if err != nil {
		t.Fatal(err)
	}
	if s.TotalTicks != worldtest.TotalTicks || s.Rate != 1 || s.Paused || !s.Raining || s.Thundering || s.Saved.IsZero() {
		t.Errorf("ReadSky = %+v", s)
	}
}

func TestReadSkyTimers(t *testing.T) {
	world := t.TempDir()
	data := filepath.Join(world, "dimensions", "minecraft", "overworld", "data", "minecraft")
	nbttest.WriteFile(t, filepath.Join(data, "world_clocks.dat"), map[string]any{
		"data": map[string]any{"minecraft:overworld": map[string]any{
			"total_ticks": int64(1000), "rate": float32(2), "paused": int8(1),
		}},
	})
	nbttest.WriteFile(t, filepath.Join(data, "weather.dat"), map[string]any{
		"data": map[string]any{
			"raining": int8(0), "thundering": int8(1),
			"rain_time": int32(600), "thunder_time": int32(1200), "clear_weather_time": int32(0),
		},
	})
	s, err := ReadSky(world)
	if err != nil {
		t.Fatal(err)
	}
	want := Sky{TotalTicks: 1000, Rate: 2, Paused: true, Thundering: true, RainTime: 600, ThunderTime: 1200, Saved: s.Saved}
	if *s != want {
		t.Errorf("ReadSky = %+v; want %+v", *s, want)
	}
}

func TestReadSkyMissingFiles(t *testing.T) {
	s, err := ReadSky(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !s.Saved.IsZero() || s.TotalTicks != 0 {
		t.Errorf("ReadSky = %+v; want an empty sky", s)
	}
}

func TestSkyAt(t *testing.T) {
	saved := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	base := Sky{TotalTicks: 5 * TicksPerDay, Rate: 1, Saved: saved}
	tests := []struct {
		name    string
		sky     Sky
		now     time.Time
		running bool
		since   time.Time
		want    Now
	}{
		{"asleep: as saved", base, saved.Add(time.Hour), false, time.Time{},
			Now{Day: 6, TimeOfDay: 0}},
		{"running: runs on from the save", base, saved.Add(10 * time.Second), true, saved.Add(-time.Hour),
			Now{Day: 6, TimeOfDay: 200, TicksPerSecond: 20, Live: true}},
		{"running: rolls into the next day", Sky{TotalTicks: 6*TicksPerDay - 100, Rate: 1, Saved: saved}, saved.Add(10 * time.Second), true, saved.Add(-time.Hour),
			Now{Day: 7, TimeOfDay: 100, TicksPerSecond: 20, Live: true}},
		{"woke after the save: runs on from the start", base, saved.Add(2*time.Hour + 5*time.Second), true, saved.Add(2 * time.Hour),
			Now{Day: 6, TimeOfDay: 100, TicksPerSecond: 20, Live: true}},
		{"no start time, fresh save", base, saved.Add(time.Minute), true, time.Time{},
			Now{Day: 6, TimeOfDay: 1200, TicksPerSecond: 20, Live: true}},
		{"no start time, old save: as saved", base, saved.Add(time.Hour), true, time.Time{},
			Now{Day: 6, TimeOfDay: 0}},
		{"paused clock", Sky{TotalTicks: 100, Rate: 1, Paused: true, Saved: saved}, saved.Add(time.Minute), true, saved,
			Now{Day: 1, TimeOfDay: 100, Live: true}},
		{"double speed", Sky{TotalTicks: 0, Rate: 2, Saved: saved}, saved.Add(time.Second), true, saved,
			Now{Day: 1, TimeOfDay: 40, TicksPerSecond: 40, Live: true}},
		{"never saved", Sky{Rate: 1}, saved, true, saved,
			Now{Day: 1}},
		{"rain starts on its timer", Sky{Rate: 1, Saved: saved, RainTime: 200, ThunderTime: 5000}, saved.Add(10 * time.Second), true, saved,
			Now{Day: 1, TimeOfDay: 200, Raining: true, TicksPerSecond: 20, Live: true}},
		{"rain not yet", Sky{Rate: 1, Saved: saved, RainTime: 201}, saved.Add(10 * time.Second), true, saved,
			Now{Day: 1, TimeOfDay: 200, TicksPerSecond: 20, Live: true}},
		{"storm ends", Sky{Rate: 1, Saved: saved, Raining: true, Thundering: true, RainTime: 100, ThunderTime: 50}, saved.Add(10 * time.Second), true, saved,
			Now{Day: 1, TimeOfDay: 200, TicksPerSecond: 20, Live: true}},
		{"thunder without rain doesn't show", Sky{Rate: 1, Saved: saved, Thundering: true}, saved, false, time.Time{},
			Now{Day: 1}},
		{"forced clear", Sky{Rate: 1, Saved: saved, Raining: true, ClearTime: 1000, RainTime: 10}, saved.Add(time.Second), true, saved,
			Now{Day: 1, TimeOfDay: 20, TicksPerSecond: 20, Live: true}},
		{"weather runs on game time, not the clock", Sky{Rate: 1, Paused: true, Saved: saved, RainTime: 100}, saved.Add(10 * time.Second), true, saved,
			Now{Day: 1, Raining: true, Live: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.sky.At(tt.now, tt.running, tt.since)
			tt.want.At = tt.now
			if got != tt.want {
				t.Errorf("At = %+v; want %+v", got, tt.want)
			}
		})
	}
}
