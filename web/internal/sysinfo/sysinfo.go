// Package sysinfo reads the host's health from Linux's /proc and /sys: CPU
// temperature, memory, load average and uptime, plus how full one
// filesystem is. Each reading is optional: a missing or unreadable source
// leaves its field nil, so the same code runs (and is tested) on machines
// without /sys/class/thermal or /proc.
package sysinfo

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// Health is one reading of the host. Nil fields couldn't be read.
type Health struct {
	TempC         *float64 `json:"temp_c,omitzero"`         // CPU temperature, °C
	Memory        *Memory  `json:"memory,omitzero"`         // RAM
	Disk          *Disk    `json:"disk,omitzero"`           // the server's filesystem
	Load          *Load    `json:"load,omitzero"`           // load averages
	UptimeSeconds *float64 `json:"uptime_seconds,omitzero"` // since boot
	CPUs          int      `json:"cpus"`                    // to judge the load against
}

// Memory is RAM in bytes. Used is Total less Available, the memory the
// kernel couldn't hand out without swapping.
type Memory struct {
	Total     uint64 `json:"total"`
	Used      uint64 `json:"used"`
	Available uint64 `json:"available"`
}

// Disk is a filesystem's size in bytes. Free is what an unprivileged
// process can still write, so Used + Free can be less than Total.
type Disk struct {
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
	Free  uint64 `json:"free"`
}

// Load is the 1, 5 and 15 minute load averages.
type Load struct {
	One     float64 `json:"one"`
	Five    float64 `json:"five"`
	Fifteen float64 `json:"fifteen"`
}

// Reader reads Health. The zero value reads the real files and the
// filesystem holding the current folder.
type Reader struct {
	// Root is prepended to /proc and /sys paths; "" is the real root.
	// Tests point it at a folder of synthetic files.
	Root string
	// DiskPath is any path on the filesystem to report; "" skips it.
	DiskPath string
	// Statfs reads a filesystem's usage; nil uses statfs(2).
	Statfs func(path string) (Disk, error)
	// Errs, if set, is told why each missing reading is missing.
	Errs func(name string, err error)
}

// Read reads everything it can. It never fails: a reading it can't get is
// left nil.
func (r *Reader) Read() Health {
	h := Health{CPUs: runtime.NumCPU()}
	if v, err := ReadTemp(r.path("sys/class/thermal/thermal_zone0/temp")); r.ok("temperature", err) {
		h.TempC = &v
	}
	if v, err := ReadMeminfo(r.path("proc/meminfo")); r.ok("memory", err) {
		h.Memory = &v
	}
	if v, err := ReadLoadavg(r.path("proc/loadavg")); r.ok("load", err) {
		h.Load = &v
	}
	if v, err := ReadUptime(r.path("proc/uptime")); r.ok("uptime", err) {
		h.UptimeSeconds = &v
	}
	if r.DiskPath != "" {
		statfs := r.Statfs
		if statfs == nil {
			statfs = Statfs
		}
		if v, err := statfs(r.DiskPath); r.ok("disk", err) {
			h.Disk = &v
		}
	}
	return h
}

func (r *Reader) path(rel string) string {
	if r.Root == "" {
		return "/" + rel
	}
	return filepath.Join(r.Root, filepath.FromSlash(rel))
}

func (r *Reader) ok(name string, err error) bool {
	if err != nil && r.Errs != nil {
		r.Errs(name, err)
	}
	return err == nil
}

// ReadTemp reads a thermal zone's temp file, which holds millidegrees
// Celsius.
func ReadTemp(path string) (float64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	milli, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	return float64(milli) / 1000, nil
}

// ReadMeminfo reads MemTotal and MemAvailable from /proc/meminfo.
func ReadMeminfo(path string) (Memory, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Memory{}, err
	}
	var total, avail uint64
	var haveTotal, haveAvail bool
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		// e.g. "MemTotal:        3884292 kB"
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		var dst *uint64
		switch name {
		case "MemTotal":
			dst, haveTotal = &total, true
		case "MemAvailable":
			dst, haveAvail = &avail, true
		default:
			continue
		}
		v, err := parseKB(rest)
		if err != nil {
			return Memory{}, fmt.Errorf("%s: %s: %w", path, name, err)
		}
		*dst = v
	}
	if !haveTotal || !haveAvail {
		return Memory{}, fmt.Errorf("%s: no MemTotal or MemAvailable", path)
	}
	if avail > total {
		return Memory{}, fmt.Errorf("%s: MemAvailable is more than MemTotal", path)
	}
	return Memory{Total: total, Used: total - avail, Available: avail}, nil
}

// parseKB parses a meminfo value such as " 3884292 kB" into bytes.
func parseKB(s string) (uint64, error) {
	f := strings.Fields(s)
	if len(f) == 0 || len(f) > 2 || (len(f) == 2 && f[1] != "kB") {
		return 0, fmt.Errorf("unexpected value %q", strings.TrimSpace(s))
	}
	v, err := strconv.ParseUint(f[0], 10, 64)
	if err != nil {
		return 0, err
	}
	if len(f) == 2 {
		v *= 1024
	}
	return v, nil
}

// ReadLoadavg reads the three load averages from /proc/loadavg, e.g.
// "0.52 0.58 0.59 1/389 12345".
func ReadLoadavg(path string) (Load, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Load{}, err
	}
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return Load{}, fmt.Errorf("%s: want 3 load averages, got %q", path, b)
	}
	var v [3]float64
	for i := range v {
		if v[i], err = parseNonNegative(f[i]); err != nil {
			return Load{}, fmt.Errorf("%s: %w", path, err)
		}
	}
	return Load{One: v[0], Five: v[1], Fifteen: v[2]}, nil
}

// ReadUptime reads seconds since boot from /proc/uptime, e.g.
// "12345.67 48000.12" (the second number is idle time).
func ReadUptime(path string) (float64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, fmt.Errorf("%s: empty", path)
	}
	v, err := parseNonNegative(f[0])
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	return v, nil
}

func parseNonNegative(s string) (float64, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("bad value %q", s)
	}
	return v, nil
}

// Statfs reports the usage of the filesystem holding path.
func Statfs(path string) (Disk, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Disk{}, &os.PathError{Op: "statfs", Path: path, Err: err}
	}
	// Bsize is a different integer type on each platform.
	bs := uint64(st.Bsize)
	if bs == 0 || st.Bfree > st.Blocks {
		return Disk{}, errors.New("statfs: implausible sizes for " + path)
	}
	return Disk{
		Total: st.Blocks * bs,
		Used:  (st.Blocks - st.Bfree) * bs,
		Free:  st.Bavail * bs,
	}, nil
}
