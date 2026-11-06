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
HugePages_Total:       0
`
	loadavg = "0.52 1.25 2.50 1/389 12345\n"
	uptime  = "93784.56 300000.12\n"
	temp    = "54768\n"
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

func fullRoot(t *testing.T) string {
	return writeFiles(t, map[string]string{
		"sys/class/thermal/thermal_zone0/temp": temp,
		"proc/meminfo":                         meminfo,
		"proc/loadavg":                         loadavg,
		"proc/uptime":                          uptime,
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
		Errs: func(name string, err error) { t.Errorf("%s: %v", name, err) },
	}
	h := r.Read()

	if h.TempC == nil || *h.TempC != 54.768 {
		t.Errorf("temp = %v, want 54.768", h.TempC)
	}
	wantMem := Memory{Total: 3884292 * 1024, Used: 1000000 * 1024, Available: 2884292 * 1024}
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
}

func TestReadMissing(t *testing.T) {
	var missing []string
	r := &Reader{
		Root:     t.TempDir(), // nothing in it
		DiskPath: "/srv/minecraft",
		Statfs:   func(string) (Disk, error) { return Disk{}, errors.New("no such filesystem") },
		Errs:     func(name string, err error) { missing = append(missing, name) },
	}
	h := r.Read()
	if h.TempC != nil || h.Memory != nil || h.Load != nil || h.UptimeSeconds != nil || h.Disk != nil {
		t.Errorf("Read with no sources = %+v, want every reading nil", h)
	}
	want := []string{"temperature", "memory", "load", "uptime", "disk"}
	if !reflect.DeepEqual(missing, want) {
		t.Errorf("reported %q, want %q", missing, want)
	}

	// No DiskPath: the disk isn't read at all.
	r = &Reader{Root: fullRoot(t), Statfs: func(string) (Disk, error) {
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
	h := (&Reader{Root: root}).Read()
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
