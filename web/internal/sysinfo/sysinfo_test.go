package sysinfo

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// A synthetic Pi: the files are made up, in the kernel's formats.
const (
	meminfo = `MemTotal:        3884292 kB
MemFree:          512000 kB
MemAvailable:    2884292 kB
Buffers:           64000 kB
Cached:          1800000 kB
SwapTotal:        204796 kB
SwapFree:         200000 kB
HugePages_Total:       0
`
	loadavg = "0.52 1.25 2.50 1/389 12345\n"
	uptime  = "93784.56 300000.12\n"
	temp    = "54768\n"
	config  = `# a made-up config.txt
dtparam=audio=on
arm_freq=1800 # comment
[pi4]
arm_freq=2000
[pi5]
arm_freq=2400
over_voltage_delta=50000
dtparam=fan_temp0=45000,fan_temp0_speed=125,i2c_arm=on
[0x1234abcd]
temp_limit=80
[EDID=XYZ-Display]
temp_limit=81
[all]
temp_soft_limit=70
[none]
arm_freq=3000
[all]
force_turbo=0
`
)

// writeFiles writes files (relative path to contents) under a new root.
func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// noVcgencmd stands in for a machine without vcgencmd, so tests never run
// a real command.
func noVcgencmd(string, ...string) ([]byte, error) {
	return nil, errors.New("vcgencmd: not found")
}

func fullRoot(t *testing.T) string {
	return writeFiles(t, map[string]string{
		"sys/class/thermal/thermal_zone0/temp": temp,
		"proc/meminfo":                         meminfo,
		"proc/loadavg":                         loadavg,
		"proc/uptime":                          uptime,
		"sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq":     "1500000\n",
		"sys/devices/system/cpu/cpu0/cpufreq/cpuinfo_max_freq":     "2400000\n",
		"sys/devices/platform/cooling_fan/hwmon/hwmon3/fan1_input": "2890\n",
		"boot/firmware/config.txt":                                 config,
	})
}

func TestRead(t *testing.T) {
	disk := Disk{Total: 64 << 30, Used: 20 << 30, Free: 41 << 30}
	var statted string
	r := &Reader{
		Root:     fullRoot(t),
		DiskPath: "/srv/minecraft",
		Statfs: func(path string) (Disk, error) {
			statted = path
			return disk, nil
		},
		Run: func(name string, args ...string) ([]byte, error) {
			if name != "vcgencmd" || !reflect.DeepEqual(args, []string{"get_throttled"}) {
				t.Errorf("ran %s %q, want vcgencmd get_throttled", name, args)
			}
			return []byte("throttled=0x50005\n"), nil
		},
		Errs: func(name string, err error) { t.Errorf("%s: %v", name, err) },
	}
	h := r.Read()

	if h.TempC == nil || *h.TempC != 54.768 {
		t.Errorf("temp = %v, want 54.768", h.TempC)
	}
	wantMem := Memory{
		Total: 3884292 * 1024, Used: 1000000 * 1024, Available: 2884292 * 1024,
		SwapTotal: 204796 * 1024, SwapUsed: 4796 * 1024,
	}
	if h.Memory == nil || *h.Memory != wantMem {
		t.Errorf("memory = %+v, want %+v", h.Memory, wantMem)
	}
	if want := (Load{0.52, 1.25, 2.5}); h.Load == nil || *h.Load != want {
		t.Errorf("load = %+v, want %+v", h.Load, want)
	}
	if h.UptimeSeconds == nil || *h.UptimeSeconds != 93784.56 {
		t.Errorf("uptime = %v, want 93784.56", h.UptimeSeconds)
	}
	if h.Disk == nil || *h.Disk != disk || statted != "/srv/minecraft" {
		t.Errorf("disk = %+v of %q, want %+v of /srv/minecraft", h.Disk, statted, disk)
	}
	if h.CPUs < 1 {
		t.Errorf("cpus = %d", h.CPUs)
	}
	if h.CPUMHz == nil || *h.CPUMHz != 1500 || h.CPUMaxMHz == nil || *h.CPUMaxMHz != 2400 {
		t.Errorf("clock = %v of %v MHz, want 1500 of 2400", h.CPUMHz, h.CPUMaxMHz)
	}
	wantThrottle := Throttle{
		Now:    ThrottleFlags{UnderVoltage: true, Throttled: true},
		Recent: ThrottleFlags{UnderVoltage: true, Throttled: true},
		Raw:    0x50005,
	}
	if h.Throttle == nil || *h.Throttle != wantThrottle {
		t.Errorf("throttle = %+v, want %+v", h.Throttle, wantThrottle)
	}
	if h.FanRPM == nil || *h.FanRPM != 2890 {
		t.Errorf("fan = %v, want 2890", h.FanRPM)
	}
	// A board filter replaces the one before it, other filters stack on
	// it without being shown, and [none] lines never apply.
	wantFirmware := []FirmwareSetting{
		{Key: "arm_freq", Value: "1800"},
		{Key: "arm_freq", Value: "2000", Model: "pi4"},
		{Key: "arm_freq", Value: "2400", Model: "pi5"},
		{Key: "over_voltage_delta", Value: "50000", Model: "pi5"},
		{Key: "fan_temp0", Value: "45000", Model: "pi5"},
		{Key: "fan_temp0_speed", Value: "125", Model: "pi5"},
		{Key: "temp_limit", Value: "80", Model: "pi5", Conditional: true},
		{Key: "temp_limit", Value: "81", Model: "pi5", Conditional: true},
		{Key: "temp_soft_limit", Value: "70"},
		{Key: "force_turbo", Value: "0"},
	}
	if !reflect.DeepEqual(h.Firmware, wantFirmware) {
		t.Errorf("firmware = %+v, want %+v", h.Firmware, wantFirmware)
	}
}

func TestReadMissing(t *testing.T) {
	var missing []string
	r := &Reader{
		Root:     t.TempDir(), // nothing in it
		DiskPath: "/srv/minecraft",
		Statfs:   func(string) (Disk, error) { return Disk{}, errors.New("no such filesystem") },
		Run:      noVcgencmd,
		Errs:     func(name string, err error) { missing = append(missing, name) },
	}
	h := r.Read()
	if !reflect.DeepEqual(h, Health{CPUs: h.CPUs}) {
		t.Errorf("Read with no sources = %+v, want every reading nil", h)
	}
	want := []string{"temperature", "memory", "load", "uptime", "cpu clock", "throttling", "fan", "config.txt", "disk"}
	if !reflect.DeepEqual(missing, want) {
		t.Errorf("reported %q, want %q", missing, want)
	}

	// No DiskPath: the disk isn't read at all.
	r = &Reader{Root: fullRoot(t), Run: noVcgencmd, Statfs: func(string) (Disk, error) {
		t.Error("Statfs called without a DiskPath")
		return Disk{}, nil
	}}
	if h := r.Read(); h.Disk != nil || h.Memory == nil {
		t.Errorf("no DiskPath: %+v, want memory and no disk", h)
	}
}

// One bad source doesn't hide the others.
func TestReadPartial(t *testing.T) {
	root := writeFiles(t, map[string]string{
		"sys/class/thermal/thermal_zone0/temp": "hot\n",
		"proc/meminfo":                         "MemTotal: 1000 kB\n", // no MemAvailable
		"proc/loadavg":                         loadavg,
		"proc/uptime":                          uptime,
	})
	h := (&Reader{Root: root, Run: noVcgencmd}).Read()
	if h.TempC != nil || h.Memory != nil {
		t.Errorf("bad temp and meminfo: %v %v, want nil", h.TempC, h.Memory)
	}
	if h.Load == nil || h.UptimeSeconds == nil {
		t.Errorf("load %v, uptime %v: want both read", h.Load, h.UptimeSeconds)
	}
}

func TestParseErrors(t *testing.T) {
	dir := t.TempDir()
	file := func(body string) string {
		p := filepath.Join(dir, "f")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	for _, body := range []string{"", "12.5\n", "abc"} {
		if v, err := ReadTemp(file(body)); err == nil {
			t.Errorf("ReadTemp(%q) = %v, want an error", body, v)
		}
	}
	if v, err := ReadTemp(file("-5000\n")); err != nil || v != -5 {
		t.Errorf("ReadTemp(-5000) = %v, %v; want -5", v, err)
	}

	for _, body := range []string{
		"",
		"MemAvailable: 10 kB\n",
		"MemTotal: lots kB\nMemAvailable: 10 kB\n",
		"MemTotal: 10 MB\nMemAvailable: 10 kB\n",
		"MemTotal: 10 kB\nMemAvailable: 20 kB\n",
		"MemTotal: 10 kB\nMemAvailable: 5 kB\nSwapTotal: 1 kB\nSwapFree: 2 kB\n",
	} {
		if v, err := ReadMeminfo(file(body)); err == nil {
			t.Errorf("ReadMeminfo(%q) = %+v, want an error", body, v)
		}
	}

	for _, body := range []string{"", "0.1 0.2", "0.1 x 0.3 1/2 3", "-1 0 0 1/2 3", "NaN 0 0 1/1 1"} {
		if v, err := ReadLoadavg(file(body)); err == nil {
			t.Errorf("ReadLoadavg(%q) = %+v, want an error", body, v)
		}
	}

	for _, body := range []string{"", "\n", "up 0.5", "-3 1"} {
		if v, err := ReadUptime(file(body)); err == nil {
			t.Errorf("ReadUptime(%q) = %v, want an error", body, v)
		}
	}
}

// Statfs works on any real folder, so the test runs on macOS too.
func TestStatfs(t *testing.T) {
	d, err := Statfs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if d.Total == 0 || d.Used > d.Total || d.Free > d.Total {
		t.Errorf("Statfs = %+v, want a plausible filesystem", d)
	}
	if _, err := Statfs(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("Statfs of a missing path: want an error")
	}
}
