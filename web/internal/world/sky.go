package world

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/nbt"
)

// TicksPerSecond is the game's speed when it keeps up.
const TicksPerSecond = 20

// Sky is the overworld clock and weather as last saved, with what's needed
// to run them forward: how fast the clock goes, and the weather timers.
type Sky struct {
	TotalTicks  int64     // overworld clock
	Rate        float64   // clock speed; 1 is normal
	Paused      bool      // the clock is stopped
	Saved       time.Time // when world_clocks.dat was written
	Raining     bool
	Thundering  bool
	RainTime    int64 // ticks until rain toggles; 0 means not yet chosen
	ThunderTime int64 // ticks until thunder toggles; 0 means not yet chosen
	ClearTime   int64 // ticks of forced clear weather left (from /weather clear)
}

// ReadSky reads the overworld clock and weather from worldDir. Both files
// are optional, as in ReadInfo; a missing clock leaves Saved zero.
func ReadSky(worldDir string) (*Sky, error) {
	data := filepath.Join(worldDir, "dimensions", "minecraft", "overworld", "data", "minecraft")
	s := &Sky{Rate: 1}

	clockPath := filepath.Join(data, "world_clocks.dat")
	if clocks, err := nbt.ReadFile(clockPath); err == nil {
		s.TotalTicks, _ = nbt.Int(clocks, "data", "minecraft:overworld", "total_ticks")
		if rate, ok := nbt.Get(clocks, "data", "minecraft:overworld", "rate").(float32); ok && rate >= 0 {
			s.Rate = float64(rate)
		}
		paused, _ := nbt.Int(clocks, "data", "minecraft:overworld", "paused")
		s.Paused = paused != 0
		if fi, err := os.Stat(clockPath); err == nil {
			s.Saved = fi.ModTime().UTC()
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("world: %w", err)
	}

	if weather, err := nbt.ReadFile(filepath.Join(data, "weather.dat")); err == nil {
		raining, _ := nbt.Int(weather, "data", "raining")
		thundering, _ := nbt.Int(weather, "data", "thundering")
		s.Raining, s.Thundering = raining != 0, thundering != 0
		s.RainTime, _ = nbt.Int(weather, "data", "rain_time")
		s.ThunderTime, _ = nbt.Int(weather, "data", "thunder_time")
		s.ClearTime, _ = nbt.Int(weather, "data", "clear_weather_time")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("world: %w", err)
	}
	return s, nil
}

// Now is the sky at one moment.
type Now struct {
	Day            int64     `json:"day"`
	TimeOfDay      int64     `json:"timeOfDay"` // 0-23999, 0 is sunrise
	Raining        bool      `json:"raining"`
	Thundering     bool      `json:"thundering"`
	TicksPerSecond float64   `json:"ticksPerSecond"` // how fast to run the clock on; 0 while it's stopped
	Live           bool      `json:"live"`           // run forward from the save, not just the save
	At             time.Time `json:"at"`             // the moment this describes
}

// freshSave is how recent a save must be to run the clock on from it when
// the server's start time is unknown: Paper saves every 5 minutes.
const freshSave = 6 * time.Minute

// At works out the sky at now. While the server runs (running, since it
// started at runningSince, zero if unknown) the clock moves on from the
// save at 20 ticks a second times its rate, and the weather follows its
// timers until the first change the save can predict. Otherwise the sky is
// as saved.
//
// Paper saves every few minutes while running, so the guess is corrected
// often. A clock saved before the server started resumed when it started,
// so without a start time only a fresh save is run forward.
func (s *Sky) At(now time.Time, running bool, runningSince time.Time) Now {
	n := Now{At: now.UTC()}
	ticks := s.TotalTicks
	raining, thundering := s.Raining, s.Thundering

	from, live := s.Saved, false
	if running && !s.Saved.IsZero() {
		switch {
		case !runningSince.IsZero():
			from, live = later(s.Saved, runningSince), true
		case now.Sub(s.Saved) <= freshSave:
			live = true
		}
	}
	if live && now.After(from) {
		elapsed := now.Sub(from).Seconds() * TicksPerSecond // game ticks, which drive the weather
		if !s.Paused {
			ticks += int64(elapsed * s.Rate)
		}
		raining, thundering = s.weatherAfter(int64(elapsed))
	}
	if live && !s.Paused {
		n.TicksPerSecond = TicksPerSecond * s.Rate
	}
	n.Live = live

	n.Day = ticks/TicksPerDay + 1
	n.TimeOfDay = ticks % TicksPerDay
	n.Raining = raining
	n.Thundering = thundering && raining // thunder only shows in rain
	return n
}

// weatherAfter runs the weather timers forward by ticks. Each timer
// toggles its weather when it runs out and then picks a random new length,
// so only the first toggle of each is known; after it, the weather stays.
func (s *Sky) weatherAfter(ticks int64) (raining, thundering bool) {
	raining, thundering = s.Raining, s.Thundering
	if s.ClearTime > 0 {
		return false, false // and what follows a forced clear spell is random
	}
	if s.RainTime > 0 && ticks >= s.RainTime {
		raining = !raining
	}
	if s.ThunderTime > 0 && ticks >= s.ThunderTime {
		thundering = !thundering
	}
	return raining, thundering
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
