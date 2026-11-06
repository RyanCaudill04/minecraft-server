package main

// GET /api/health: the Pi's temperature, memory, disk, load and uptime,
// and Paper's TPS and MSPT while it's awake.

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/mcstatus"
	"github.com/ryancaudill04/minecraft-server/web/internal/rcon"
	"github.com/ryancaudill04/minecraft-server/web/internal/sysinfo"
)

const (
	healthMaxAge = 10 * time.Second
	tpsTimeout   = 3 * time.Second
)

// Health is the JSON body of GET /api/health. Readings the Pi can't give
// are left out.
type Health struct {
	sysinfo.Health
	Paper   PaperHealth `json:"paper"`
	Checked time.Time   `json:"checked"`
}

// PaperHealth is how Paper is keeping up. TPS and MSPT come over RCON, so
// they're only there while Paper is awake; admin never wakes it to ask.
type PaperHealth struct {
	State string `json:"state"` // the status ping's state: active, sleeping, starting or offline
	TPS   *TPS   `json:"tps,omitzero"`
	MSPT  *MSPT  `json:"mspt,omitzero"`
	// Note says why TPS is missing while Paper is active.
	Note string `json:"note,omitzero"`
}

// TPS is ticks per second averaged over 1, 5 and 15 minutes; 20 is full
// speed.
type TPS struct {
	One     float64 `json:"one"`
	Five    float64 `json:"five"`
	Fifteen float64 `json:"fifteen"`
}

// MSPT is milliseconds per tick over Window (such as "1m"); above 50 the
// server can't keep 20 TPS.
type MSPT struct {
	Window string  `json:"window,omitzero"`
	Avg    float64 `json:"avg"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
}

const (
	noteNoRCON      = "RCON isn't configured, so TPS can't be read."
	noteUnreachable = "RCON isn't answering."
	noteFailed      = "Couldn't read TPS over RCON."
)

// loadHealth reads the Pi and, if the status ping says Paper is awake,
// asks it for TPS and MSPT. It never fails: what it can't read is left out.
// site.Cache holds its lock while this runs, so errs needs no lock.
func (a *app) loadHealth(ctx context.Context, host *sysinfo.Reader) (Health, error) {
	h := Health{Health: host.Read(), Checked: time.Now().UTC()}

	s, err := a.status.Get(ctx)
	if err != nil {
		h.Paper.State = "unknown"
		return h, nil
	}
	h.Paper.State = s.State
	if s.State != mcstatus.StateActive {
		return h, nil
	}
	if a.rcon == nil {
		h.Paper.Note = noteNoRCON
		return h, nil
	}

	ctx, cancel := context.WithTimeout(ctx, tpsTimeout)
	defer cancel()
	out, err := a.rcon(ctx, "tps")
	a.healthErrs.log("admin: health: tps", err)
	switch {
	case errors.Is(err, rcon.ErrUnreachable):
		// Paper stopped since the ping, or is between restarts.
		h.Paper.Note = noteUnreachable
		return h, nil
	case err != nil:
		h.Paper.Note = noteFailed
		return h, nil
	}
	if t, ok := parseTPS(stripFormatting(out)); ok {
		h.Paper.TPS = &t
	} else {
		h.Paper.Note = noteFailed
	}
	// mspt is Paper's own command; on a server without it, there's no MSPT.
	if out, err := a.rcon(ctx, "mspt"); err == nil {
		if m, ok := parseMSPT(stripFormatting(out)); ok {
			h.Paper.MSPT = &m
		}
	}
	return h, nil
}

var (
	number   = regexp.MustCompile(`\d+(?:\.\d+)?`)
	triple   = regexp.MustCompile(`(\d+(?:\.\d+)?)/(\d+(?:\.\d+)?)/(\d+(?:\.\d+)?)`)
	tpsLine  = regexp.MustCompile(`(?m)TPS from last [^:]*:(.*)$`)
	msptHead = regexp.MustCompile(`from last ([^:]*):`)
)

// parseTPS reads the output of Paper's tps command, with formatting
// stripped: "TPS from last 1m, 5m, 15m: 19.98, *20.0, 20.0". A * marks a
// value Paper capped at 20.
func parseTPS(out string) (TPS, bool) {
	m := tpsLine.FindStringSubmatch(out)
	if m == nil {
		return TPS{}, false
	}
	nums := number.FindAllString(m[1], -1)
	if len(nums) < 3 {
		return TPS{}, false
	}
	var v [3]float64
	for i := range v {
		f, err := strconv.ParseFloat(nums[i], 64)
		if err != nil {
			return TPS{}, false
		}
		v[i] = f
	}
	return TPS{One: v[0], Five: v[1], Fifteen: v[2]}, true
}

// parseMSPT reads the output of Paper's mspt command, with formatting
// stripped:
//
//	Server tick times (avg/min/max) from last 5s, 10s, 1m:
//	◴ 4.5/2.1/10.3, 4.4/2.0/12.1, 4.6/1.9/15.0
//
// and returns the longest window, the last one.
func parseMSPT(out string) (MSPT, bool) {
	ts := triple.FindAllStringSubmatch(out, -1)
	if len(ts) == 0 {
		return MSPT{}, false
	}
	last := ts[len(ts)-1]
	var v [3]float64
	for i := range v {
		f, err := strconv.ParseFloat(last[i+1], 64)
		if err != nil {
			return MSPT{}, false
		}
		v[i] = f
	}
	m := MSPT{Avg: v[0], Min: v[1], Max: v[2]}
	if h := msptHead.FindStringSubmatch(out); h != nil {
		windows := strings.Split(h[1], ",")
		if len(windows) == len(ts) {
			m.Window = strings.TrimSpace(windows[len(windows)-1])
		}
	}
	return m, true
}
