package sysinfo

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Counters is one reading of the host's running totals. On its own it
// says little; Diff two readings for rates over the time between them.
// Nil or empty fields couldn't be read.
type Counters struct {
	At    time.Time
	CPU   *CPUTimes  // every core together
	Cores []CPUTimes // each core, in order
	// Net is bytes in and out of each physical network interface
	// (eth0, wlan0), by name. Loopback and virtual interfaces (VPNs,
	// containers) are left out, since their traffic also crosses a
	// physical one.
	Net  map[string]NetCounters
	Disk *DiskCounters // the disk holding Reader.DiskPath
	// Services is the CPU time each running service in Reader.Services has
	// used, in microseconds.
	Services map[string]uint64
}

// CPUTimes is time a CPU has spent since boot, in clock ticks (USER_HZ,
// 100 a second on Linux). Busy is user, nice, system, irq, softirq and
// steal time; IOWait is idle time spent waiting for the disk.
type CPUTimes struct {
	Busy, Idle, IOWait uint64
}

// NetCounters is bytes received and sent since boot.
type NetCounters struct {
	Rx, Tx uint64
}

// DiskCounters is bytes read and written since boot on one block device,
// such as mmcblk0p2 or nvme0n1p2.
type DiskCounters struct {
	Device      string
	Read, Write uint64
}

// Counters reads every running total it can. It never fails: what it
// can't read is left out.
func (r *Reader) Counters() Counters {
	c := Counters{At: time.Now()}
	if total, cores, err := ReadStat(r.path("proc/stat")); r.ok("cpu", err) {
		c.CPU, c.Cores = &total, cores
	}
	if v, err := r.readNet(); r.ok("network", err) {
		c.Net = v
	}
	if r.DiskPath != "" {
		if v, err := r.readDiskIO(); r.ok("disk io", err) {
			c.Disk = &v
		}
	}
	c.Services = r.serviceCPU()
	return c
}

// ReadStat reads the cpu lines of /proc/stat: the first is every core
// together, then cpu0, cpu1 and so on.
//
//	cpu  10132153 290696 3084719 46828483 16683 0 25195 0 0 0
//	cpu0 1393280 32966 572056 13343292 6130 0 17875 0 0 0
//
// The columns are user, nice, system, idle, iowait, irq, softirq, steal,
// guest and guest_nice. Guest time is already counted in user, so it's
// skipped.
func ReadStat(path string) (CPUTimes, []CPUTimes, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return CPUTimes{}, nil, err
	}
	var total *CPUTimes
	var cores []CPUTimes
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 || !strings.HasPrefix(f[0], "cpu") {
			continue
		}
		t, err := parseCPU(f[1:])
		if err != nil {
			return CPUTimes{}, nil, fmt.Errorf("%s: %s: %w", path, f[0], err)
		}
		if f[0] == "cpu" {
			total = &t
			continue
		}
		// Offline cores have no line, so the numbers can skip; they're
		// kept in order and the gaps closed.
		if _, err := strconv.Atoi(f[0][3:]); err != nil {
			return CPUTimes{}, nil, fmt.Errorf("%s: unexpected line %q", path, f[0])
		}
		cores = append(cores, t)
	}
	if err := sc.Err(); err != nil {
		return CPUTimes{}, nil, fmt.Errorf("%s: %w", path, err)
	}
	if total == nil {
		return CPUTimes{}, nil, fmt.Errorf("%s: no cpu line", path)
	}
	return *total, cores, nil
}

func parseCPU(f []string) (CPUTimes, error) {
	if len(f) < 4 {
		return CPUTimes{}, fmt.Errorf("want at least 4 columns, got %d", len(f))
	}
	var v [8]uint64
	for i := 0; i < len(f) && i < len(v); i++ {
		n, err := strconv.ParseUint(f[i], 10, 64)
		if err != nil {
			return CPUTimes{}, err
		}
		v[i] = n
	}
	const user, nice, system, idle, iowait, irq, softirq, steal = 0, 1, 2, 3, 4, 5, 6, 7
	return CPUTimes{
		Busy:   v[user] + v[nice] + v[system] + v[irq] + v[softirq] + v[steal],
		Idle:   v[idle],
		IOWait: v[iowait],
	}, nil
}

// readNet reads /proc/net/dev and keeps the physical interfaces: those
// with a device link in /sys/class/net. Without /sys/class/net it keeps
// everything but loopback.
func (r *Reader) readNet() (map[string]NetCounters, error) {
	all, err := ReadNetDev(r.path("proc/net/dev"))
	if err != nil {
		return nil, err
	}
	_, err = os.Stat(r.path("sys/class/net"))
	haveSys := err == nil
	out := map[string]NetCounters{}
	for name, c := range all {
		if name == "lo" {
			continue
		}
		if haveSys {
			if _, err := os.Stat(r.path("sys/class/net/" + name + "/device")); err != nil {
				continue
			}
		}
		out[name] = c
	}
	return out, nil
}

// ReadNetDev reads bytes received and sent by every interface from
// /proc/net/dev: two header lines, then one line an interface.
//
//	eth0: 9876543 8000 0 0 0 0 0 0 1234567 6000 0 0 0 0 0 0
//
// Received bytes are the first column after the colon and sent bytes the
// ninth.
func ReadNetDev(path string) (map[string]NetCounters, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]NetCounters{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue // a header
		}
		name = strings.TrimSpace(name)
		f := strings.Fields(rest)
		if len(f) < 9 {
			return nil, fmt.Errorf("%s: %s: want 16 columns, got %d", path, name, len(f))
		}
		rx, err1 := strconv.ParseUint(f[0], 10, 64)
		tx, err2 := strconv.ParseUint(f[8], 10, 64)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("%s: %s: bad byte counts %q, %q", path, name, f[0], f[8])
		}
		out[name] = NetCounters{Rx: rx, Tx: tx}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return out, nil
}

func (r *Reader) readDiskIO() (DiskCounters, error) {
	device := r.Device
	if device == nil {
		device = Device
	}
	major, minor, err := device(r.DiskPath)
	if err != nil {
		return DiskCounters{}, err
	}
	return ReadDiskstats(r.path("proc/diskstats"), major, minor)
}

// ReadDiskstats reads bytes read and written by one device from
// /proc/diskstats, found by its major and minor numbers:
//
//	179       2 mmcblk0p2 41785 9721 2793082 31446 39815 52741 4523704 186730 0 ...
//
// After the name come reads, reads merged, sectors read, time reading,
// writes, writes merged and sectors written. A sector there is always 512
// bytes, whatever the device's own sector size.
func ReadDiskstats(path string, major, minor uint32) (DiskCounters, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return DiskCounters{}, err
	}
	want := [2]string{strconv.FormatUint(uint64(major), 10), strconv.FormatUint(uint64(minor), 10)}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 10 || f[0] != want[0] || f[1] != want[1] {
			continue
		}
		read, err1 := strconv.ParseUint(f[5], 10, 64)
		written, err2 := strconv.ParseUint(f[9], 10, 64)
		if err1 != nil || err2 != nil {
			return DiskCounters{}, fmt.Errorf("%s: %s: bad sector counts %q, %q", path, f[2], f[5], f[9])
		}
		return DiskCounters{Device: f[2], Read: read * 512, Write: written * 512}, nil
	}
	if err := sc.Err(); err != nil {
		return DiskCounters{}, fmt.Errorf("%s: %w", path, err)
	}
	return DiskCounters{}, fmt.Errorf("%s: no device %d:%d", path, major, minor)
}

// Usage is the rates between two Counters. Nil fields couldn't be worked
// out: missing from either reading, or a counter that went backwards
// (a service restarted, or an interface reset).
type Usage struct {
	Seconds float64   `json:"seconds"` // the time between the readings
	CPU     *CPUUsage `json:"cpu,omitzero"`
	Net     *NetRate  `json:"net,omitzero"`
	Disk    *DiskRate `json:"disk,omitzero"`
	// Services is the CPU each service used, in cores: 1.5 is one and a
	// half cores busy the whole time.
	Services map[string]float64 `json:"services,omitzero"`
}

// CPUUsage is the share of time the CPU was busy, 0 to 100. IOWait is the
// share it sat idle waiting for the disk.
type CPUUsage struct {
	Percent float64   `json:"percent"`
	IOWait  float64   `json:"iowait"`
	Cores   []float64 `json:"cores,omitzero"` // each core's Percent
}

// NetRate is bytes a second in and out, over every physical interface.
type NetRate struct {
	In  float64 `json:"in"`
	Out float64 `json:"out"`
}

// DiskRate is bytes a second read from and written to one device.
type DiskRate struct {
	Device string  `json:"device"`
	Read   float64 `json:"read"`
	Write  float64 `json:"write"`
}

// Diff returns the rates from prev to cur. cur must be the later reading.
func Diff(prev, cur Counters) Usage {
	secs := cur.At.Sub(prev.At).Seconds()
	if secs <= 0 {
		return Usage{}
	}
	u := Usage{Seconds: secs}

	if prev.CPU != nil && cur.CPU != nil {
		if c, ok := cpuPercent(*prev.CPU, *cur.CPU); ok {
			u.CPU = &c
			if len(prev.Cores) == len(cur.Cores) {
				u.CPU.Cores = make([]float64, len(cur.Cores))
				for i := range cur.Cores {
					core, _ := cpuPercent(prev.Cores[i], cur.Cores[i])
					u.CPU.Cores[i] = core.Percent
				}
			}
		}
	}

	var in, out uint64
	var counted bool
	for name, c := range cur.Net {
		p, ok := prev.Net[name]
		if !ok || c.Rx < p.Rx || c.Tx < p.Tx {
			continue
		}
		in += c.Rx - p.Rx
		out += c.Tx - p.Tx
		counted = true
	}
	if counted {
		u.Net = &NetRate{In: float64(in) / secs, Out: float64(out) / secs}
	}

	if p, c := prev.Disk, cur.Disk; p != nil && c != nil && p.Device == c.Device && c.Read >= p.Read && c.Write >= p.Write {
		u.Disk = &DiskRate{
			Device: c.Device,
			Read:   float64(c.Read-p.Read) / secs,
			Write:  float64(c.Write-p.Write) / secs,
		}
	}

	for name, c := range cur.Services {
		p, ok := prev.Services[name]
		if !ok || c < p {
			continue
		}
		if u.Services == nil {
			u.Services = map[string]float64{}
		}
		u.Services[name] = float64(c-p) / 1e6 / secs
	}
	return u
}

// cpuPercent is the busy and iowait shares of the ticks between two
// readings. A column can step back a tick on some kernels (iowait is
// notorious), so each difference stops at zero. With no ticks between
// them it reports false.
func cpuPercent(prev, cur CPUTimes) (CPUUsage, bool) {
	busy := sub(cur.Busy, prev.Busy)
	idle := sub(cur.Idle, prev.Idle)
	iowait := sub(cur.IOWait, prev.IOWait)
	total := busy + idle + iowait
	if total == 0 {
		return CPUUsage{}, false
	}
	return CPUUsage{
		Percent: 100 * float64(busy) / float64(total),
		IOWait:  100 * float64(iowait) / float64(total),
	}, true
}

func sub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}
