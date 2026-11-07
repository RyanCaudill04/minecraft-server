package sysinfo

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ReadKHz reads a cpufreq file, which holds kHz, as MHz.
func ReadKHz(path string) (float64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	khz, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	return float64(khz) / 1000, nil
}

// readThrottle asks the firmware for its throttling flags with vcgencmd.
// Kernels before 6.12 also had a get_throttled file in sysfs, but it was
// removed, so vcgencmd (in the video group) is the way that keeps working.
func (r *Reader) readThrottle() (Throttle, error) {
	run := r.Run
	if run == nil {
		run = runCommand
	}
	out, err := run("vcgencmd", "get_throttled")
	if err != nil {
		return Throttle{}, fmt.Errorf("vcgencmd get_throttled: %w", err)
	}
	return ParseThrottled(string(out))
}

// runCommand runs a short command, giving up after 2 seconds.
func runCommand(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

// ParseThrottled reads vcgencmd get_throttled's output, such as
// "throttled=0x50005": bits in hex. Bits 0 to 3 are under-voltage,
// frequency capped, throttled and soft temperature limit now; bits 16 to
// 19 are the same since they were last cleared. The kernel's hwmon driver
// clears them every 2 seconds, so they mean "just now", not since boot.
func ParseThrottled(out string) (Throttle, error) {
	s, ok := strings.CutPrefix(strings.TrimSpace(out), "throttled=")
	if !ok {
		return Throttle{}, fmt.Errorf("unexpected get_throttled output %q", out)
	}
	v, err := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 32)
	if err != nil {
		return Throttle{}, fmt.Errorf("get_throttled: %w", err)
	}
	bits := uint32(v)
	flags := func(shift uint) ThrottleFlags {
		return ThrottleFlags{
			UnderVoltage:  bits&(1<<shift) != 0,
			FreqCapped:    bits&(1<<(shift+1)) != 0,
			Throttled:     bits&(1<<(shift+2)) != 0,
			SoftTempLimit: bits&(1<<(shift+3)) != 0,
		}
	}
	return Throttle{Now: flags(0), Recent: flags(16), Raw: bits}, nil
}

// fanGlob finds the fan's speed on a Pi 5 with the active cooler (the
// pwm-fan driver's hwmon device).
const fanGlob = "sys/devices/platform/cooling_fan/hwmon/hwmon*/fan1_input"

func (r *Reader) readFan() (float64, error) {
	paths, err := filepath.Glob(r.path(fanGlob))
	if err != nil {
		return 0, err
	}
	if len(paths) == 0 {
		return 0, errors.New("no fan (no active cooler?)")
	}
	b, err := os.ReadFile(paths[0])
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", paths[0], err)
	}
	return float64(v), nil
}

// FirmwareSetting is one setting from the Pi's config.txt that bears on
// its clocks, voltage, temperature limits or fan. A key that isn't set
// means the firmware's default.
type FirmwareSetting struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	// Model is the board filter the line is under, such as "pi5"; "" for
	// every board. The firmware only applies it on a board it matches.
	Model string `json:"model,omitzero"`
	// Conditional is true when the line is also under another kind of
	// filter (a serial number, a display, a GPIO pin, tryboot), so it may
	// not apply. Those filters aren't shown, since some identify the board
	// or what's plugged into it.
	Conditional bool `json:"conditional,omitzero"`
}

// modelFilter matches config.txt's board filters: [pi4], [pi400], [pi5],
// [pi3+], [pi0w], [cm4], [cm5], [board-type=0x17] and the like.
var modelFilter = regexp.MustCompile(`^(?:(?:pi|cm)[0-9]+[a-z+]*|board-type=.+)$`)

// firmwareKeys are the config.txt settings worth showing for the Pi's
// health. dtparam lines are kept for the fan_temp* curve only.
var firmwareKeys = map[string]bool{
	"arm_freq": true, "arm_freq_min": true, "arm_boost": true,
	"core_freq": true, "core_freq_min": true, "gpu_freq": true, "sdram_freq": true,
	"over_voltage": true, "over_voltage_delta": true, "over_voltage_min": true,
	"force_turbo": true, "initial_turbo": true,
	"temp_limit": true, "temp_soft_limit": true,
}

// configPaths are where config.txt is: /boot/firmware since Raspberry Pi
// OS bookworm, /boot before it.
var configPaths = []string{"boot/firmware/config.txt", "boot/config.txt"}

func (r *Reader) readFirmware() ([]FirmwareSetting, error) {
	var err error
	for _, rel := range configPaths {
		var s []FirmwareSetting
		if s, err = ReadConfigTxt(r.path(rel)); err == nil {
			return s, nil
		}
	}
	return nil, err
}

// ReadConfigTxt reads the clock, voltage, temperature and fan settings
// from a Raspberry Pi config.txt, in file order:
//
//	[pi5]
//	arm_freq=2800
//	over_voltage_delta=50000
//	dtparam=fan_temp0=45000,fan_temp0_speed=125
//
// Filters of different kinds stack until [all] resets them, and a board
// filter replaces the board filter before it. Lines under [none] never
// apply, so they're left out. It doesn't follow include lines.
func ReadConfigTxt(path string) ([]FirmwareSetting, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := []FirmwareSetting{}
	model, conditional := "", false
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line, _, _ := strings.Cut(sc.Text(), "#")
		line = strings.TrimSpace(line)
		if f, ok := strings.CutPrefix(line, "["); ok {
			switch f = strings.TrimSpace(strings.TrimSuffix(f, "]")); {
			case f == "all":
				model, conditional = "", false
			case f == "none" || modelFilter.MatchString(f):
				model = f
			default:
				conditional = true
			}
			continue
		}
		if model == "none" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		add := func(k, v string) {
			out = append(out, FirmwareSetting{Key: k, Value: v, Model: model, Conditional: conditional})
		}
		if key == "dtparam" {
			for _, p := range strings.Split(value, ",") {
				k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
				if strings.HasPrefix(k, "fan_temp") {
					add(k, v)
				}
			}
			continue
		}
		if firmwareKeys[key] {
			add(key, value)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return out, nil
}
