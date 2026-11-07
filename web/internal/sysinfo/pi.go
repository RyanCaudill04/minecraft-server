package sysinfo

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// throttleGlob finds the firmware's get_throttled file, which is under
// soc:firmware on a Pi 4 and soc@<address>:firmware on a Pi 5.
const throttleGlob = "sys/devices/platform/soc*/*firmware/get_throttled"

func (r *Reader) readThrottle() (Throttle, error) {
	paths, err := filepath.Glob(r.path(throttleGlob))
	if err != nil {
		return Throttle{}, err
	}
	if len(paths) == 0 {
		return Throttle{}, errors.New("no get_throttled file (not a Raspberry Pi?)")
	}
	return ReadThrottled(paths[0])
}

// ReadThrottled reads the firmware's get_throttled file: bits in hex, the
// same number vcgencmd get_throttled prints. Bits 0 to 3 are under-voltage,
// frequency capped, throttled and soft temperature limit now; bits 16 to
// 19 are the same since boot.
func ReadThrottled(path string) (Throttle, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Throttle{}, err
	}
	s := strings.TrimPrefix(strings.TrimSpace(string(b)), "0x")
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return Throttle{}, fmt.Errorf("%s: %w", path, err)
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
	return Throttle{Now: flags(0), SinceBoot: flags(16), Raw: bits}, nil
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
	// Section is the [filter] the line is under, such as "pi5"; "" for the
	// top of the file or [all]. The firmware only applies a filtered line
	// on a board it matches.
	Section string `json:"section,omitzero"`
}

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
// It doesn't follow include lines.
func ReadConfigTxt(path string) ([]FirmwareSetting, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := []FirmwareSetting{}
	section := ""
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line, _, _ := strings.Cut(sc.Text(), "#")
		line = strings.TrimSpace(line)
		if f, ok := strings.CutPrefix(line, "["); ok {
			section = strings.TrimSpace(strings.TrimSuffix(f, "]"))
			if section == "all" {
				section = ""
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if key == "dtparam" {
			for _, p := range strings.Split(value, ",") {
				k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
				if strings.HasPrefix(k, "fan_temp") {
					out = append(out, FirmwareSetting{Key: k, Value: v, Section: section})
				}
			}
			continue
		}
		if firmwareKeys[key] {
			out = append(out, FirmwareSetting{Key: key, Value: value, Section: section})
		}
	}
	return out, nil
}
