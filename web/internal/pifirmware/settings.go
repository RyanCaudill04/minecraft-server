// Package pifirmware changes a Raspberry Pi 5's clock, voltage,
// temperature-limit and fan-curve settings in config.txt, safely enough to
// do from a web page on a Pi nobody can reach by hand.
//
// Only a few settings are allowed, each with a range (see Limits). They
// are written as typed numbers into one block that this package owns,
// between "# BEGIN admin" and "# END admin" at the end of config.txt; the
// rest of the file is never rewritten.
//
// New settings never go straight into config.txt. Stage writes them to
// tryboot.txt, which the firmware reads instead of config.txt for one boot
// only, after "reboot '0 tryboot'". If that boot hangs or crashes, the
// hardware watchdog resets the Pi and it comes back on config.txt. If it
// boots, Boot checks it's healthy, and only Keep copies the settings into
// config.txt. See Host.
//
// The writes need root, so they run in the pi-firmware command, which
// admin starts through one sudoers rule. Reading the state doesn't: admin
// calls Host.Status itself.
package pifirmware

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Settings are the config.txt settings admin may change. A nil field
// isn't written, so the firmware uses its default.
type Settings struct {
	// ArmFreq is the CPU's top clock in MHz (arm_freq).
	ArmFreq *int `json:"arm_freq,omitempty"`
	// OverVoltageDelta is added to the CPU's voltage, in microvolts
	// (over_voltage_delta).
	OverVoltageDelta *int `json:"over_voltage_delta,omitempty"`
	// TempLimit is the temperature in °C at which the firmware drops the
	// clocks back to their defaults (temp_limit).
	TempLimit *int `json:"temp_limit,omitempty"`
	// Fan is the cooling fan's curve: four steps, coolest first
	// (dtparam=fan_temp0..3).
	Fan *FanCurve `json:"fan,omitempty"`
}

// FanCurve is the Pi 5 fan's four steps, coolest first.
type FanCurve [4]FanStep

// FanStep is one step of the fan curve: the fan runs at Speed once the CPU
// reaches Temp, and drops back once it cools to Temp-Hyst.
type FanStep struct {
	Temp  int `json:"temp"`  // millidegrees C (fan_tempN)
	Hyst  int `json:"hyst"`  // millidegrees C (fan_tempN_hyst)
	Speed int `json:"speed"` // PWM, 0 to 255 (fan_tempN_speed)
}

// Range is the allowed values of one setting, and the firmware's default.
type Range struct {
	Min     int `json:"min"`
	Max     int `json:"max"`
	Default int `json:"default"`
}

func (r Range) check(name string, v int) error {
	if v < r.Min || v > r.Max {
		return fmt.Errorf("%s must be between %d and %d, not %d", name, r.Min, r.Max, v)
	}
	return nil
}

// LimitSet is every setting's range, for a Raspberry Pi 5.
type LimitSet struct {
	ArmFreq          Range    `json:"arm_freq"`
	OverVoltageDelta Range    `json:"over_voltage_delta"`
	TempLimit        Range    `json:"temp_limit"`
	FanTemp          Range    `json:"fan_temp"`
	FanHyst          Range    `json:"fan_hyst"`
	FanSpeed         Range    `json:"fan_speed"`
	FanDefault       FanCurve `json:"fan_default"`
	// FanTopSpeed is the least the hottest step may run the fan at, so
	// the curve can't turn the fan off for good.
	FanTopSpeed int `json:"fan_top_speed"`
	// ArmFreqWarn is the clock above which the page warns that many Pis
	// aren't stable.
	ArmFreqWarn int `json:"arm_freq_warn"`
}

// Limits are the Pi 5's ranges. The firmware's defaults are from the
// config.txt documentation and the overlays README; the ranges are kept
// well inside what the firmware accepts.
var Limits = LimitSet{
	// Below the 1500 MHz arm_freq_min isn't supported.
	ArmFreq: Range{Min: 1500, Max: 3000, Default: 2400},
	// 0 to 50 mV.
	OverVoltageDelta: Range{Min: 0, Max: 50000, Default: 0},
	// The firmware clamps anything above 85.
	TempLimit: Range{Min: 60, Max: 85, Default: 85},
	FanTemp:   Range{Min: 30000, Max: 80000},
	FanHyst:   Range{Min: 1000, Max: 10000, Default: 5000},
	FanSpeed:  Range{Min: 0, Max: 255},
	FanDefault: FanCurve{
		{Temp: 50000, Hyst: 5000, Speed: 75},
		{Temp: 60000, Hyst: 5000, Speed: 125},
		{Temp: 67500, Hyst: 5000, Speed: 175},
		{Temp: 75000, Hyst: 5000, Speed: 250},
	},
	FanTopSpeed: 125,
	ArmFreqWarn: 2800,
}

// Validate reports the first setting that's out of range, or nil.
func (s Settings) Validate() error {
	l := Limits
	if s.ArmFreq != nil {
		if err := l.ArmFreq.check("arm_freq", *s.ArmFreq); err != nil {
			return err
		}
	}
	if s.OverVoltageDelta != nil {
		if err := l.OverVoltageDelta.check("over_voltage_delta", *s.OverVoltageDelta); err != nil {
			return err
		}
	}
	if s.TempLimit != nil {
		if err := l.TempLimit.check("temp_limit", *s.TempLimit); err != nil {
			return err
		}
	}
	if s.Fan != nil {
		for i, st := range s.Fan {
			for _, c := range []struct {
				r    Range
				name string
				v    int
			}{
				{l.FanTemp, "fan_temp%d", st.Temp},
				{l.FanHyst, "fan_temp%d_hyst", st.Hyst},
				{l.FanSpeed, "fan_temp%d_speed", st.Speed},
			} {
				if err := c.r.check(fmt.Sprintf(c.name, i), c.v); err != nil {
					return err
				}
			}
			if i == 0 {
				continue
			}
			if prev := s.Fan[i-1]; st.Temp <= prev.Temp {
				return fmt.Errorf("fan_temp%d must be hotter than fan_temp%d", i, i-1)
			} else if st.Speed < prev.Speed {
				return fmt.Errorf("fan_temp%d_speed can't be slower than fan_temp%d_speed", i, i-1)
			}
		}
		if s.Fan[3].Speed < l.FanTopSpeed {
			return fmt.Errorf("fan_temp3_speed must be at least %d, so the fan still runs when it's hot", l.FanTopSpeed)
		}
	}
	return nil
}

// Empty reports whether nothing is set, so every setting is the default.
func (s Settings) Empty() bool {
	return s.ArmFreq == nil && s.OverVoltageDelta == nil && s.TempLimit == nil && s.Fan == nil
}

// The block's marker lines. Each must be a whole line.
const (
	beginMarker = "# BEGIN admin"
	endMarker   = "# END admin"
	blockNote   = "# Written by pi-firmware from admin. Change these from admin, not by hand."
)

// TrialWatchdog is how long, in seconds, the OS has to start serving the
// hardware watchdog on a trial boot (kernel_watchdog_timeout). Only
// tryboot.txt gets it, so a trial that hangs while booting resets back to
// config.txt.
const TrialWatchdog = 60

// Block renders s as the admin block, ready to splice into config.txt.
// trial adds the watchdog line tryboot.txt needs. Every line is built here
// from numbers, so nothing from a request is copied into the file. Block
// returns nil when there's nothing to write.
func Block(s Settings, trial bool) []byte {
	if s.Empty() && !trial {
		return nil
	}
	var b bytes.Buffer
	b.WriteString(beginMarker + "\n" + blockNote + "\n[all]\n")
	if s.ArmFreq != nil {
		fmt.Fprintf(&b, "arm_freq=%d\n", *s.ArmFreq)
	}
	if s.OverVoltageDelta != nil {
		fmt.Fprintf(&b, "over_voltage_delta=%d\n", *s.OverVoltageDelta)
	}
	if s.TempLimit != nil {
		fmt.Fprintf(&b, "temp_limit=%d\n", *s.TempLimit)
	}
	if s.Fan != nil {
		for i, st := range s.Fan {
			fmt.Fprintf(&b, "dtparam=fan_temp%d=%d,fan_temp%d_hyst=%d,fan_temp%d_speed=%d\n",
				i, st.Temp, i, st.Hyst, i, st.Speed)
		}
	}
	if trial {
		fmt.Fprintf(&b, "kernel_watchdog_timeout=%d\n", TrialWatchdog)
	}
	b.WriteString(endMarker + "\n")
	return b.Bytes()
}

// ErrHandEdited means the admin block isn't one Block wrote, so it's left
// alone rather than guessed at.
var ErrHandEdited = errors.New("the admin block in config.txt was changed by hand: fix or remove it on the Pi first")

// blockSpan finds the admin block in config's lines: the index of its
// BEGIN and END lines, or -1, -1 if there isn't one. It's an error for
// the markers not to pair up, or for settings to follow the block, since
// the block's [all] would change which boards they apply to.
func blockSpan(lines []string) (begin, end int, err error) {
	begin, end = -1, -1
	for i, l := range lines {
		switch strings.TrimSpace(l) {
		case beginMarker:
			if begin >= 0 {
				return 0, 0, errors.New("config.txt has more than one admin block")
			}
			begin = i
		case endMarker:
			if begin < 0 || end >= 0 {
				return 0, 0, errors.New("config.txt has an END admin line without a BEGIN admin line before it")
			}
			end = i
		}
	}
	if begin >= 0 && end < 0 {
		return 0, 0, errors.New("config.txt has a BEGIN admin line without an END admin line")
	}
	if end >= 0 {
		for _, l := range lines[end+1:] {
			if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "#") {
				return 0, 0, errors.New("config.txt has settings after the admin block: move them above it on the Pi first")
			}
		}
	}
	return begin, end, nil
}

func splitLines(b []byte) []string {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// Splice returns config with its admin block replaced by block, or the
// block added at the end if there isn't one. A nil block removes it.
// Every other line is kept as it is, except that line endings become \n
// (the firmware reads both).
func Splice(config, block []byte) ([]byte, error) {
	lines := splitLines(config)
	begin, end, err := blockSpan(lines)
	if err != nil {
		return nil, err
	}
	var kept []string
	if begin < 0 {
		kept = lines
	} else {
		kept = append(append(kept, lines[:begin]...), lines[end+1:]...)
		// Drop the blank lines that separated the old block.
		for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
			kept = kept[:len(kept)-1]
		}
	}
	var b bytes.Buffer
	for _, l := range kept {
		b.WriteString(l + "\n")
	}
	if block != nil {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.Write(block)
	}
	return b.Bytes(), nil
}

// ParseBlock reads the settings back from config's admin block. found is
// false if there isn't one. trial reports whether it has the trial-only
// watchdog line. Anything Block wouldn't have written is ErrHandEdited.
func ParseBlock(config []byte) (s Settings, found, trial bool, err error) {
	lines := splitLines(config)
	begin, end, err := blockSpan(lines)
	if err != nil || begin < 0 {
		return Settings{}, false, false, err
	}
	var fan FanCurve
	fanSeen := 0
	sawAll := false
	seen := map[string]bool{}
	for _, l := range lines[begin+1 : end] {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if !sawAll {
			if l != "[all]" {
				return Settings{}, true, false, ErrHandEdited
			}
			sawAll = true
			continue
		}
		key, value, ok := strings.Cut(l, "=")
		if !ok {
			return Settings{}, true, false, ErrHandEdited
		}
		if key != "dtparam" {
			if seen[key] {
				return Settings{}, true, false, ErrHandEdited
			}
			seen[key] = true
		}
		switch key {
		case "arm_freq", "over_voltage_delta", "temp_limit", "kernel_watchdog_timeout":
			v, err := strconv.Atoi(value)
			if err != nil || strconv.Itoa(v) != value {
				return Settings{}, true, false, ErrHandEdited
			}
			switch key {
			case "arm_freq":
				s.ArmFreq = &v
			case "over_voltage_delta":
				s.OverVoltageDelta = &v
			case "temp_limit":
				s.TempLimit = &v
			case "kernel_watchdog_timeout":
				if v != TrialWatchdog {
					return Settings{}, true, false, ErrHandEdited
				}
				trial = true
			}
		case "dtparam":
			var i int
			var st FanStep
			n, err := fmt.Sscanf(value, "fan_temp%d=%d,fan_temp%d_hyst=%d,fan_temp%d_speed=%d",
				&i, &st.Temp, new(int), &st.Hyst, new(int), &st.Speed)
			if err != nil || n != 6 || i != fanSeen || i > 3 {
				return Settings{}, true, false, ErrHandEdited
			}
			// Render it again: Sscanf skips spaces and ignores the step
			// numbers it reads into new(int), so the line must match
			// exactly what Block writes.
			want := fmt.Sprintf("fan_temp%d=%d,fan_temp%d_hyst=%d,fan_temp%d_speed=%d", i, st.Temp, i, st.Hyst, i, st.Speed)
			if value != want {
				return Settings{}, true, false, ErrHandEdited
			}
			fan[i] = st
			fanSeen++
		default:
			return Settings{}, true, false, ErrHandEdited
		}
	}
	switch fanSeen {
	case 0:
	case 4:
		s.Fan = &fan
	default:
		return Settings{}, true, false, ErrHandEdited
	}
	if err := s.Validate(); err != nil {
		return Settings{}, true, false, fmt.Errorf("%w (%v)", ErrHandEdited, err)
	}
	return s, true, trial, nil
}
