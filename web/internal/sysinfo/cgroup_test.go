package sysinfo

import (
	"reflect"
	"strings"
	"testing"
)

func TestServiceMemory(t *testing.T) {
	var errs []string
	r := &Reader{
		Root: writeFiles(t, map[string]string{
			"proc/self/cgroup": "0::/user.slice/user-1000.slice/user@1000.service/app.slice/admin.service\n",
			// Paper with 10 GiB in use and 2 GiB of world files in the
			// page cache that the kernel can drop.
			cgroupSlice + "/minecraft.service/memory.current": "12884901888\n",
			cgroupSlice + "/minecraft.service/memory.stat":    "anon 10737418240\nfile 2147483648\ninactive_file 2147483648\nactive_file 0\n",
			cgroupSlice + "/admin.service/memory.current":     "20000000\n",
			cgroupSlice + "/admin.service/memory.stat":        "anon 18000000\ninactive_file 2000000\n",
			// More inactive cache than memory.current (they're read a
			// moment apart): 0, not a wrapped-around number.
			cgroupSlice + "/play.service/memory.current":   "1000\n",
			cgroupSlice + "/play.service/memory.stat":      "inactive_file 4096\n",
			cgroupSlice + "/broken.service/memory.current": "lots\n",
			// No memory controller: cpu.stat is always there, memory files
			// aren't.
			cgroupSlice + "/nomem.service/cpu.stat": "usage_usec 5\n",
			// Template instances are in a slice of their own.
			cgroupSlice + "/app-site.slice/site@portfolio.service/memory.current": "9000000\n",
			cgroupSlice + "/app-site.slice/site@portfolio.service/memory.stat":    "inactive_file 1000000\n",
		}),
		Services: []string{"minecraft.service", "mc-backup.service", "admin.service", "play.service", "site@portfolio.service", "site@other.service", "broken.service", "nomem.service", "../user.slice", ""},
		Run:      noVcgencmd,
		Errs:     func(name string, err error) { errs = append(errs, name) },
	}
	got := r.Read().Services
	want := []ServiceMemory{
		{Name: "minecraft.service", Memory: 10 << 30},
		{Name: "admin.service", Memory: 18000000},
		{Name: "play.service", Memory: 0},
		{Name: "site@portfolio.service", Memory: 8000000},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("services = %+v, want %+v", got, want)
	}
	// mc-backup.service and site@other.service aren't running, so they
	// have no cgroup: not an error. nomem.service is running, so its
	// missing memory.current is.
	var serviceErrs []string
	for _, e := range errs {
		if strings.HasPrefix(e, "service") {
			serviceErrs = append(serviceErrs, e)
		}
	}
	if want := []string{"service broken.service", "service nomem.service", "service ../user.slice", "service "}; !reflect.DeepEqual(serviceErrs, want) {
		t.Errorf("reported %q, want %q", serviceErrs, want)
	}
}

func TestCgroupDir(t *testing.T) {
	for _, tc := range []struct {
		cgroup string
		want   string // under Root; "" for an error
	}{
		{"0::/user.slice/user-1000.slice/user@1000.service/app.slice/admin.service\n", "sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice"},
		// A hybrid system lists v1 controllers first.
		{"12:cpu,cpuacct:/user.slice\n0::/system.slice/admin.service\n", "sys/fs/cgroup/system.slice"},
		{"0::/\n", ""},
		{"0::\n", ""},
		{"12:cpu,cpuacct:/user.slice\n", ""},
	} {
		root := writeFiles(t, map[string]string{"proc/self/cgroup": tc.cgroup})
		r := &Reader{Root: root}
		got, err := r.cgroupDir()
		switch {
		case tc.want == "" && err == nil:
			t.Errorf("cgroupDir(%q) = %q, want an error", tc.cgroup, got)
		case tc.want != "" && (err != nil || got != r.path(tc.want)):
			t.Errorf("cgroupDir(%q) = %q, %v; want %q", tc.cgroup, got, err, r.path(tc.want))
		}
	}
}

func TestReadCgroupErrors(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"nostat/memory.current":  "100\n",
		"badstat/memory.current": "100\n",
		"badstat/memory.stat":    "inactive_file many\n",
		"nokey/memory.current":   "100\n",
		"nokey/memory.stat":      "anon 100\n",
		"nokey/cpu.stat":         "user_usec 5\n",
		"badcpu/cpu.stat":        "usage_usec -1\n",
	})
	for _, sub := range []string{"nostat", "badstat", "nokey", "missing"} {
		if v, err := ReadCgroupMemory(dir + "/" + sub); err == nil {
			t.Errorf("ReadCgroupMemory(%s) = %d, want an error", sub, v)
		}
	}
	for _, sub := range []string{"nokey", "badcpu", "missing"} {
		if v, err := ReadCgroupCPU(dir + "/" + sub); err == nil {
			t.Errorf("ReadCgroupCPU(%s) = %d, want an error", sub, v)
		}
	}
}

func TestSystemdEscape(t *testing.T) {
	for in, want := range map[string]string{
		"site":      "site",
		"my-site":   `my\x2dsite`,
		"a.b:c_d":   "a.b:c_d",
		".hidden":   `\x2ehidden`,
		"two words": `two\x20words`,
	} {
		if got := systemdEscape(in); got != want {
			t.Errorf("systemdEscape(%q) = %q, want %q", in, got, want)
		}
	}
}
