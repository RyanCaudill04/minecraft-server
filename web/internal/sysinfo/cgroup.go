package sysinfo

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// Each systemd unit runs in its own cgroup (v2), a folder under
// /sys/fs/cgroup whose files count the CPU time and memory of every process
// in the unit. minecraft.service's cgroup holds both lazymc and Paper.

// cgroupDir is the folder holding the cgroups of this process's sibling
// units: the parent of its own cgroup, read from /proc/self/cgroup. For a
// systemd user unit such as admin.service that's the slice every other
// user unit runs in too, such as
// /sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice.
func (r *Reader) cgroupDir() (string, error) {
	p := r.path("proc/self/cgroup")
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	// cgroup v2 has a single line, "0::/path".
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		own, ok := strings.CutPrefix(sc.Text(), "0::")
		if !ok {
			continue
		}
		own = path.Clean("/" + own)
		if own == "/" {
			return "", fmt.Errorf("%s: in the root cgroup, with no slice to look in", p)
		}
		return r.path(path.Join("sys/fs/cgroup", path.Dir(own))), nil
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("%s: %w", p, err)
	}
	return "", fmt.Errorf("%s: no cgroup v2 entry", p)
}

// services calls f with the cgroup folder of each service in r.Services
// that is running. A stopped service has no cgroup folder, so it's skipped
// quietly; anything else wrong, including a file missing from a folder
// that's there, is reported through Errs.
func (r *Reader) services(f func(name, dir string) error) {
	if len(r.Services) == 0 {
		return
	}
	parent, err := r.cgroupDir()
	if !r.ok("services", err) {
		return
	}
	for _, name := range r.Services {
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
			r.ok("service "+name, errors.New("not a unit name"))
			continue
		}
		dir, err := unitCgroup(parent, name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if r.ok("service "+name, err) {
			r.ok("service "+name, f(name, dir))
		}
	}
}

// unitCgroup finds a running unit's cgroup folder in parent. systemd puts
// each instance of a template unit in a slice of its own, so
// site@example.service is in app-site.slice/ rather than next to the
// other units.
func unitCgroup(parent, name string) (string, error) {
	dir := filepath.Join(parent, name)
	if prefix, _, ok := strings.Cut(name, "@"); ok {
		dir = filepath.Join(parent, "app-"+systemdEscape(prefix)+".slice", name)
	}
	st, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%s: not a folder", dir)
	}
	return dir, nil
}

// systemdEscape escapes a unit name prefix the way systemd does for slice
// names (unit_name_escape): letters, digits, ':', '_' and '.' are kept
// (but not a leading '.'), '/' becomes '-', and anything else becomes
// \xNN.
func systemdEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '/':
			b.WriteByte('-')
		case c == '.' && i > 0, c == ':', c == '_',
			'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, `\x%02x`, c)
		}
	}
	return b.String()
}

func (r *Reader) serviceMemory() []ServiceMemory {
	var out []ServiceMemory
	r.services(func(name, dir string) error {
		v, err := ReadCgroupMemory(dir)
		if err == nil {
			out = append(out, ServiceMemory{Name: name, Memory: v})
		}
		return err
	})
	return out
}

func (r *Reader) serviceCPU() map[string]uint64 {
	var out map[string]uint64
	r.services(func(name, dir string) error {
		v, err := ReadCgroupCPU(dir)
		if err == nil {
			if out == nil {
				out = map[string]uint64{}
			}
			out[name] = v
		}
		return err
	})
	return out
}

// ReadCgroupMemory reads a cgroup's memory.current, less the inactive_file
// page cache in its memory.stat. Paper reads and writes world files all
// the time, and without that the page cache would make it look far bigger
// than its heap.
func ReadCgroupMemory(dir string) (uint64, error) {
	cur, err := readUint(filepath.Join(dir, "memory.current"))
	if err != nil {
		return 0, err
	}
	inactive, err := readKey(filepath.Join(dir, "memory.stat"), "inactive_file")
	if err != nil {
		return 0, err
	}
	if inactive > cur {
		return 0, nil
	}
	return cur - inactive, nil
}

// ReadCgroupCPU reads the CPU time a cgroup has used, in microseconds,
// from usage_usec in its cpu.stat.
func ReadCgroupCPU(dir string) (uint64, error) {
	return readKey(filepath.Join(dir, "cpu.stat"), "usage_usec")
}

func readUint(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	return v, nil
}

// readKey reads one value from a cgroup file of "key value" lines.
func readKey(path, key string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), " ")
		if !ok || k != key {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: %s: %w", path, key, err)
		}
		return n, nil
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	return 0, fmt.Errorf("%s: no %s", path, key)
}
