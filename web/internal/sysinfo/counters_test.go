package sysinfo

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

const (
	stat = `cpu  1000 50 250 7000 100 10 40 0 30 0
cpu0 500 25 125 3500 50 5 20 0 15 0
cpu1 500 25 125 3500 50 5 20 0 15 0
intr 123456 0 0
ctxt 987654
btime 1700000000
`
	netdev = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:  555000    100    0    0    0     0          0         0   555000     100    0    0    0     0       0          0
  eth0: 9000000   8000    0    0    0     0          0         0  3000000    6000    0    0    0     0       0          0
 wlan0:   1000     10    0    0    0     0          0         0     2000      20    0    0    0     0       0          0
tailscale0: 777000 70 0 0 0 0 0 0 888000 80 0 0 0 0 0 0
`
	diskstats = `   1       0 ram0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0
 179       0 mmcblk0 50000 10000 4000000 30000 40000 50000 9000000 180000 0 50000 210000 0 0 0 0 0 0
 179       2 mmcblk0p2 41785 9721 2793082 31446 39815 52741 4523704 186730 0 54210 218180 0 0 0 0 0 0
`
)

func countersRoot(t *testing.T) string {
	return writeFiles(t, map[string]string{
		"proc/stat":                                 stat,
		"proc/net/dev":                              netdev,
		"proc/diskstats":                            diskstats,
		"sys/class/net/lo/operstate":                "unknown\n",
		"sys/class/net/eth0/device/uevent":          "",
		"sys/class/net/wlan0/device/uevent":         "",
		"sys/class/net/tailscale0/operstate":        "unknown\n",
		"proc/self/cgroup":                          "0::/user.slice/user-1000.slice/user@1000.service/app.slice/admin.service\n",
		cgroupSlice + "/minecraft.service/cpu.stat": "usage_usec 123456789\nuser_usec 100000000\nsystem_usec 23456789\n",
	})
}

const cgroupSlice = "sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice"

func TestCounters(t *testing.T) {
	var device string
	r := &Reader{
		Root:     countersRoot(t),
		DiskPath: "/srv/minecraft",
		Device: func(path string) (uint32, uint32, error) {
			device = path
			return 179, 2, nil
		},
		Services: []string{"minecraft.service", "mc-backup.service"}, // the backup isn't running
		Errs:     func(name string, err error) { t.Errorf("%s: %v", name, err) },
	}
	before := time.Now()
	c := r.Counters()

	if c.At.Before(before) {
		t.Errorf("At = %v, want after %v", c.At, before)
	}
	if want := (CPUTimes{Busy: 1350, Idle: 7000, IOWait: 100}); c.CPU == nil || *c.CPU != want {
		t.Errorf("cpu = %+v, want %+v", c.CPU, want)
	}
	if want := []CPUTimes{{675, 3500, 50}, {675, 3500, 50}}; !reflect.DeepEqual(c.Cores, want) {
		t.Errorf("cores = %+v, want %+v", c.Cores, want)
	}
	wantNet := map[string]NetCounters{"eth0": {Rx: 9000000, Tx: 3000000}, "wlan0": {Rx: 1000, Tx: 2000}}
	if !reflect.DeepEqual(c.Net, wantNet) {
		t.Errorf("net = %+v, want %+v (no lo or tailscale0)", c.Net, wantNet)
	}
	wantDisk := DiskCounters{Device: "mmcblk0p2", Read: 2793082 * 512, Write: 4523704 * 512}
	if c.Disk == nil || *c.Disk != wantDisk || device != "/srv/minecraft" {
		t.Errorf("disk = %+v of %q, want %+v of /srv/minecraft", c.Disk, device, wantDisk)
	}
	if want := map[string]uint64{"minecraft.service": 123456789}; !reflect.DeepEqual(c.Services, want) {
		t.Errorf("services = %v, want %v", c.Services, want)
	}
}

func TestCountersMissing(t *testing.T) {
	var missing []string
	r := &Reader{
		Root:     t.TempDir(),
		DiskPath: "/srv/minecraft",
		Device:   func(string) (uint32, uint32, error) { return 0, 0, errors.New("no device") },
		Services: []string{"minecraft.service"},
		Errs:     func(name string, err error) { missing = append(missing, name) },
	}
	c := r.Counters()
	if c.CPU != nil || c.Cores != nil || c.Net != nil || c.Disk != nil || c.Services != nil {
		t.Errorf("Counters with no sources = %+v, want nothing read", c)
	}
	if want := []string{"cpu", "network", "disk io", "services"}; !reflect.DeepEqual(missing, want) {
		t.Errorf("reported %q, want %q", missing, want)
	}

	// No DiskPath and no Services: neither is looked for.
	r = &Reader{Root: countersRoot(t), Device: func(string) (uint32, uint32, error) {
		t.Error("Device called without a DiskPath")
		return 0, 0, nil
	}}
	if c := r.Counters(); c.Disk != nil || c.Services != nil || c.CPU == nil {
		t.Errorf("no DiskPath or Services: %+v, want only cpu and net", c)
	}
}

// Without /sys/class/net to tell physical interfaces apart, everything but
// loopback counts.
func TestCountersNetWithoutSys(t *testing.T) {
	r := &Reader{Root: writeFiles(t, map[string]string{"proc/net/dev": netdev})}
	c := r.Counters()
	_, lo := c.Net["lo"]
	if len(c.Net) != 3 || lo || c.Net["tailscale0"].Rx != 777000 {
		t.Errorf("net = %+v, want eth0, wlan0 and tailscale0", c.Net)
	}
}

func TestReadStat(t *testing.T) {
	file := func(body string) string {
		return writeFiles(t, map[string]string{"stat": body}) + "/stat"
	}

	// Old kernels have only four columns. Offline cores have no line.
	total, cores, err := ReadStat(file("cpu 10 20 30 40\ncpu0 1 2 3 4\ncpu3 9 0 0 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if total != (CPUTimes{Busy: 60, Idle: 40}) || len(cores) != 2 || cores[1] != (CPUTimes{Busy: 9, Idle: 1}) {
		t.Errorf("ReadStat = %+v, %+v", total, cores)
	}

	for _, body := range []string{
		"",
		"cpu0 1 2 3 4\n", // no total
		"cpu 1 2 3\n",    // too few columns
		"cpu 1 2 x 4\n",  // not a number
		"cpu 1 2 3 4\ncpux 1 2 3 4\n",
	} {
		if _, _, err := ReadStat(file(body)); err == nil {
			t.Errorf("ReadStat(%q): want an error", body)
		}
	}
}

func TestParseCountersErrors(t *testing.T) {
	file := func(body string) string {
		return writeFiles(t, map[string]string{"f": body}) + "/f"
	}
	for _, body := range []string{
		"eth0: 1 2 3\n",
		"eth0: x 0 0 0 0 0 0 0 5 0 0 0 0 0 0 0\n",
	} {
		if v, err := ReadNetDev(file(body)); err == nil {
			t.Errorf("ReadNetDev(%q) = %+v, want an error", body, v)
		}
	}
	if v, err := ReadNetDev(file("Inter-|\n face |\n")); err != nil || len(v) != 0 {
		t.Errorf("ReadNetDev of headers only = %+v, %v; want nothing", v, err)
	}

	if v, err := ReadDiskstats(file(diskstats), 8, 1); err == nil {
		t.Errorf("ReadDiskstats of a missing device = %+v, want an error", v)
	}
	if v, err := ReadDiskstats(file(" 8 1 sda1 1 2 x 4 5 6 7 8\n"), 8, 1); err == nil {
		t.Errorf("ReadDiskstats of a bad line = %+v, want an error", v)
	}
	// The whole disk and its partition are told apart by minor number.
	if v, err := ReadDiskstats(file(diskstats), 179, 0); err != nil || v.Device != "mmcblk0" {
		t.Errorf("ReadDiskstats(179:0) = %+v, %v; want mmcblk0", v, err)
	}
}

func TestDiff(t *testing.T) {
	t0 := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	prev := Counters{
		At:       t0,
		CPU:      &CPUTimes{Busy: 1000, Idle: 5000, IOWait: 100},
		Cores:    []CPUTimes{{500, 2500, 50}, {500, 2500, 50}},
		Net:      map[string]NetCounters{"eth0": {Rx: 1000, Tx: 500}, "wlan0": {Rx: 10, Tx: 10}},
		Disk:     &DiskCounters{Device: "mmcblk0p2", Read: 4096, Write: 8192},
		Services: map[string]uint64{"minecraft.service": 10_000_000, "admin.service": 500_000},
	}
	cur := Counters{
		At: t0.Add(2 * time.Second),
		// 200 ticks: 100 busy, 80 idle, 20 iowait.
		CPU: &CPUTimes{Busy: 1100, Idle: 5080, IOWait: 120},
		// Core 0 all busy, core 1 all idle.
		Cores: []CPUTimes{{600, 2500, 50}, {500, 2600, 50}},
		Net: map[string]NetCounters{
			"eth0":  {Rx: 3000, Tx: 1500},
			"wlan0": {Rx: 5, Tx: 5},     // reset: left out
			"usb0":  {Rx: 9999, Tx: 99}, // new: left out
		},
		Disk: &DiskCounters{Device: "mmcblk0p2", Read: 4096 + 1<<20, Write: 8192 + 2<<20},
		Services: map[string]uint64{
			"minecraft.service": 13_000_000, // 3 s of CPU in 2 s: 1.5 cores
			"admin.service":     100_000,    // restarted: left out
			"play.service":      42,         // started: left out
		},
	}
	got := Diff(prev, cur)
	want := Usage{
		Seconds:  2,
		CPU:      &CPUUsage{Percent: 50, IOWait: 10, Cores: []float64{100, 0}},
		Net:      &NetRate{In: 1000, Out: 500},
		Disk:     &DiskRate{Device: "mmcblk0p2", Read: 1 << 19, Write: 1 << 20},
		Services: map[string]float64{"minecraft.service": 1.5},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Diff =\n%+v\nwant\n%+v", show(got), show(want))
	}
}

func TestDiffNothing(t *testing.T) {
	t0 := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	cpu := &CPUTimes{Busy: 10, Idle: 10}

	// Readings out of order, or at the same instant, give no rates.
	if u := Diff(Counters{At: t0, CPU: cpu}, Counters{At: t0, CPU: cpu}); !reflect.DeepEqual(u, Usage{}) {
		t.Errorf("Diff at the same time = %+v, want nothing", u)
	}

	// A field missing from either reading is left out, and so is a CPU
	// with no ticks between readings.
	prev := Counters{At: t0, CPU: cpu, Cores: []CPUTimes{{1, 1, 0}}, Disk: &DiskCounters{Device: "sda1"}}
	cur := Counters{At: t0.Add(time.Second), CPU: cpu, Cores: []CPUTimes{{1, 1, 0}, {1, 1, 0}}, Disk: &DiskCounters{Device: "sdb1", Read: 5}}
	if u := Diff(prev, cur); !reflect.DeepEqual(u, Usage{Seconds: 1}) {
		t.Errorf("Diff = %+v, want only Seconds", show(u))
	}

	// A core list that changed length (a core went offline) leaves the
	// total but no per-core figures.
	cur.CPU = &CPUTimes{Busy: 20, Idle: 20}
	if u := Diff(prev, cur); u.CPU == nil || u.CPU.Percent != 50 || u.CPU.Cores != nil {
		t.Errorf("Diff with a core gone = %+v, want 50%% and no cores", u.CPU)
	}

	// A column stepping back a tick doesn't make a negative or huge share.
	prev.CPU = &CPUTimes{Busy: 100, Idle: 100, IOWait: 50}
	cur.CPU = &CPUTimes{Busy: 150, Idle: 150, IOWait: 49}
	if u := Diff(prev, cur); u.CPU == nil || u.CPU.Percent != 50 || u.CPU.IOWait != 0 {
		t.Errorf("Diff with iowait going back = %+v, want 50%% and no iowait", u.CPU)
	}
}

// show makes a Usage readable in a failure message.
func show(u Usage) map[string]any {
	m := map[string]any{"seconds": u.Seconds, "services": u.Services}
	if u.CPU != nil {
		m["cpu"] = *u.CPU
	}
	if u.Net != nil {
		m["net"] = *u.Net
	}
	if u.Disk != nil {
		m["disk"] = *u.Disk
	}
	return m
}
