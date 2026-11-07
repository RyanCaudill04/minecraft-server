package pifirmware

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePi is a folder laid out like a Pi 5's /boot/firmware, /proc, /sys
// and /var, with synthetic files, and a record of the commands run.
type fakePi struct {
	*Host
	t    *testing.T
	mu   sync.Mutex
	runs [][]string
}

func newFakePi(t *testing.T) *fakePi {
	t.Helper()
	root := t.TempDir()
	p := &fakePi{t: t}
	p.Host = &Host{
		Root: root,
		Now:  func() time.Time { return time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC) },
		Run: func(_ context.Context, name string, args ...string) error {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.runs = append(p.runs, append([]string{name}, args...))
			return nil
		},
	}
	p.write(modelPath, "Raspberry Pi 5 Model B Rev 1.0\x00")
	p.write(ConfigPath, stock)
	p.write(watchdogState, "active\n")
	p.write(bootUnitLink, "")
	for _, cpu := range []string{"policy0"} {
		p.write("sys/devices/system/cpu/cpufreq/"+cpu+"/scaling_governor", "ondemand\n")
		p.write("sys/devices/system/cpu/cpufreq/"+cpu+"/scaling_available_governors", "conservative ondemand userspace powersave performance schedutil \n")
	}
	p.write("sys/devices/platform/soc@107c000000/soc@107c000000:firmware/get_throttled", "0\n")
	p.setTrialBoot(false)
	return p
}

func (p *fakePi) write(rel, content string) {
	p.t.Helper()
	path := filepath.Join(p.Root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		p.t.Fatal(err)
	}
}

func (p *fakePi) read(rel string) string {
	p.t.Helper()
	b, err := os.ReadFile(filepath.Join(p.Root, rel))
	if err != nil {
		p.t.Fatal(err)
	}
	return string(b)
}

func (p *fakePi) exists(rel string) bool {
	_, err := os.Lstat(filepath.Join(p.Root, rel))
	return err == nil
}

// setTrialBoot sets the device tree's tryboot flag, as the firmware does.
func (p *fakePi) setTrialBoot(on bool) {
	b := make([]byte, 4)
	if on {
		binary.BigEndian.PutUint32(b, 1)
	}
	p.write(trybootFlag, string(b))
}

func (p *fakePi) commands() [][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.runs)
}

func (p *fakePi) phase() Phase {
	p.t.Helper()
	tr, err := p.readState()
	if err != nil {
		p.t.Fatal(err)
	}
	if tr == nil {
		return ""
	}
	return tr.Phase
}

func refused(err error) bool {
	var r *RefusedError
	return errors.As(err, &r)
}

var oc = Settings{ArmFreq: new(2600), OverVoltageDelta: new(25000)}

// The whole happy path: stage, reboot onto tryboot.txt, pass the check,
// keep. config.txt changes only at Keep, after a backup.
func TestTrialKept(t *testing.T) {
	p := newFakePi(t)
	ctx := context.Background()

	if err := p.Stage(oc); err != nil {
		t.Fatal(err)
	}
	if p.read(ConfigPath) != stock {
		t.Fatal("Stage changed config.txt")
	}
	st := p.Status()
	if st.Pending == nil || !reflect.DeepEqual(*st.Pending, oc) || !st.Saved.Empty() {
		t.Fatalf("after Stage: pending %+v, saved %+v", st.Pending, st.Saved)
	}

	if err := p.RebootTrial(ctx); err != nil {
		t.Fatal(err)
	}
	if got := p.commands(); !reflect.DeepEqual(got, [][]string{{"systemctl", "reboot", "0 tryboot"}}) {
		t.Fatalf("ran %q", got)
	}
	if p.phase() != PhaseRebooting {
		t.Fatalf("phase %q", p.phase())
	}

	// The firmware boots tryboot.txt once.
	p.setTrialBoot(true)
	checked := false
	p.Healthy = func(context.Context) error { checked = true; return nil }
	p.HealthWait = time.Second
	if err := p.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	if !checked || p.phase() != PhasePassed {
		t.Fatalf("after Boot: checked %v, phase %q", checked, p.phase())
	}
	// Nothing else may be staged or booted until it's kept or dropped.
	if err := p.Stage(Settings{ArmFreq: new(2800)}); !refused(err) {
		t.Errorf("Stage during a trial = %v, want refused", err)
	}
	if err := p.RebootTrial(ctx); !refused(err) {
		t.Errorf("RebootTrial during a trial = %v, want refused", err)
	}

	if err := p.Keep(); err != nil {
		t.Fatal(err)
	}
	if p.exists(TrybootPath) {
		t.Error("Keep left tryboot.txt")
	}
	config := p.read(ConfigPath)
	if !strings.HasPrefix(config, stock) || strings.Contains(config, "kernel_watchdog_timeout") {
		t.Errorf("config.txt after Keep:\n%s", config)
	}
	if s, _, _, _ := ParseBlock([]byte(config)); !reflect.DeepEqual(s, oc) {
		t.Errorf("config.txt has %+v, want %+v", s, oc)
	}
	if got := p.read(filepath.Join(BackupDir, "config-20261107T120000Z.txt")); got != stock {
		t.Errorf("backup = %q, want the old config.txt", got)
	}
	if p.phase() != PhaseKept {
		t.Errorf("phase %q", p.phase())
	}
	if len(p.commands()) != 1 {
		t.Errorf("ran %q after the trial reboot", p.commands()[1:])
	}
}

// A check that fails on the trial boot drops tryboot.txt and reboots
// back onto config.txt, which is never touched.
func TestTrialFailsCheck(t *testing.T) {
	for name, setup := range map[string]func(p *fakePi){
		"admin down": func(p *fakePi) {
			p.Healthy = func(context.Context) error { return errors.New("connection refused") }
		},
		"under-voltage": func(p *fakePi) {
			p.write("sys/devices/platform/soc@107c000000/soc@107c000000:firmware/get_throttled", "0x50000\n")
		},
		"cpu": func(p *fakePi) {
			p.Load = func(context.Context) error { return errors.New("a CPU computed a wrong hash") }
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := newFakePi(t)
			ctx := context.Background()
			if err := p.Stage(oc); err != nil {
				t.Fatal(err)
			}
			if err := p.RebootTrial(ctx); err != nil {
				t.Fatal(err)
			}
			p.setTrialBoot(true)
			p.HealthWait = 50 * time.Millisecond
			setup(p)
			if err := p.Boot(ctx); err != nil {
				t.Fatal(err)
			}
			if p.phase() != PhaseFailed {
				t.Errorf("phase %q", p.phase())
			}
			if p.exists(TrybootPath) {
				t.Error("tryboot.txt is still there")
			}
			if p.read(ConfigPath) != stock {
				t.Error("config.txt changed")
			}
			want := [][]string{{"systemctl", "reboot", "0 tryboot"}, {"systemctl", "reboot"}}
			if got := p.commands(); !reflect.DeepEqual(got, want) {
				t.Errorf("ran %q, want %q", got, want)
			}
			if err := p.Keep(); !refused(err) {
				t.Errorf("Keep after a failed trial = %v, want refused", err)
			}
		})
	}
}

// The firmware's own fallback: the trial hung or reset, so the next boot
// is a normal one on config.txt. Boot records that and clears up.
func TestTrialResetFallsBack(t *testing.T) {
	for _, phase := range []Phase{PhaseRebooting, PhaseChecking, PhasePassed} {
		p := newFakePi(t)
		if err := p.Stage(oc); err != nil {
			t.Fatal(err)
		}
		if err := p.writeState(&Trial{Phase: phase, Settings: oc}); err != nil {
			t.Fatal(err)
		}
		if err := p.Boot(context.Background()); err != nil {
			t.Fatal(err)
		}
		want := PhaseFailed
		if phase == PhasePassed {
			want = PhaseExpired
		}
		if p.phase() != want || p.exists(TrybootPath) || p.read(ConfigPath) != stock {
			t.Errorf("from %s: phase %q, tryboot.txt %v", phase, p.phase(), p.exists(TrybootPath))
		}
		if len(p.commands()) != 0 {
			t.Errorf("from %s: ran %q", phase, p.commands())
		}
	}
}

func TestDiscardDuringTrial(t *testing.T) {
	p := newFakePi(t)
	ctx := context.Background()
	if err := p.Stage(oc); err != nil {
		t.Fatal(err)
	}
	if err := p.RebootTrial(ctx); err != nil {
		t.Fatal(err)
	}
	p.setTrialBoot(true)
	if err := p.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.Discard(); err != nil {
		t.Fatal(err)
	}
	if p.exists(TrybootPath) || p.phase() != PhaseDiscarded {
		t.Errorf("tryboot.txt %v, phase %q", p.exists(TrybootPath), p.phase())
	}
	if err := p.Keep(); !refused(err) {
		t.Errorf("Keep after Discard = %v, want refused", err)
	}
	// Staging again is allowed once it's dropped.
	if err := p.Stage(Settings{TempLimit: new(80)}); err != nil {
		t.Errorf("Stage after Discard: %v", err)
	}
}

func TestRefusals(t *testing.T) {
	ctx := context.Background()

	// The server must be stopped, checked by the helper too.
	p := newFakePi(t)
	p.ServerUp = func() bool { return true }
	p.Stage(oc)
	if err := p.RebootTrial(ctx); !refused(err) {
		t.Errorf("RebootTrial with the server up = %v, want refused", err)
	}
	if err := p.Reboot(ctx); !refused(err) {
		t.Errorf("Reboot with the server up = %v, want refused", err)
	}
	if len(p.commands()) != 0 {
		t.Errorf("ran %q", p.commands())
	}

	p = newFakePi(t)
	p.write(modelPath, "Raspberry Pi 4 Model B Rev 1.4\x00")
	if err := p.Stage(oc); !refused(err) {
		t.Errorf("Stage on a Pi 4 = %v, want refused", err)
	}

	p = newFakePi(t)
	if err := p.Stage(Settings{ArmFreq: new(3500)}); !refused(err) || p.exists(TrybootPath) {
		t.Errorf("Stage out of range = %v", err)
	}
	if err := p.RebootTrial(ctx); !refused(err) {
		t.Errorf("RebootTrial with nothing staged = %v, want refused", err)
	}
	if err := p.Keep(); !refused(err) {
		t.Errorf("Keep without a trial = %v, want refused", err)
	}

	// No watchdog, no trial: a hang couldn't reset itself.
	p = newFakePi(t)
	p.write(watchdogState, "inactive\n")
	if err := p.Stage(oc); err != nil {
		t.Fatal(err)
	}
	if err := p.RebootTrial(ctx); !refused(err) || len(p.commands()) != 0 {
		t.Errorf("RebootTrial without a watchdog = %v, ran %q", err, p.commands())
	}

	// A tryboot.txt admin didn't write is left alone.
	p = newFakePi(t)
	p.write(TrybootPath, stock+"arm_freq=3000\n")
	if err := p.RebootTrial(ctx); !refused(err) || len(p.commands()) != 0 {
		t.Errorf("RebootTrial of someone else's tryboot.txt = %v", err)
	}

	// A hand-edited block isn't overwritten.
	p = newFakePi(t)
	p.write(ConfigPath, stock+"# BEGIN admin\n[all]\nforce_turbo=1\n# END admin\n")
	if err := p.Stage(oc); !refused(err) || p.exists(TrybootPath) {
		t.Errorf("Stage with a hand-edited block = %v", err)
	}
	if st := p.Status(); len(st.Problems) == 0 {
		t.Error("Status didn't report the hand-edited block")
	}
}

// Symlinks planted where admin's files go are never followed.
func TestSymlinksRefused(t *testing.T) {
	p := newFakePi(t)
	target := filepath.Join(t.TempDir(), "shadow")
	os.WriteFile(target, []byte("secret"), 0o644)

	if err := os.Symlink(target, filepath.Join(p.Root, TrybootPath)); err != nil {
		t.Fatal(err)
	}
	if err := p.Stage(oc); err == nil {
		t.Error("Stage wrote through a tryboot.txt symlink")
	}
	if b, _ := os.ReadFile(target); string(b) != "secret" {
		t.Errorf("the symlink's target changed to %q", b)
	}

	p = newFakePi(t)
	os.Remove(filepath.Join(p.Root, ConfigPath))
	os.Symlink(target, filepath.Join(p.Root, ConfigPath))
	if err := p.Stage(oc); err == nil {
		t.Error("Stage read config.txt through a symlink")
	}
}

func TestBackupsKeepNewest(t *testing.T) {
	p := newFakePi(t)
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	p.Now = func() time.Time { return now }
	for i := range 8 {
		now = now.Add(time.Hour)
		if err := p.backup([]byte{byte('a' + i)}); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(p.Root, BackupDir))
	if len(entries) != keepBackups {
		t.Fatalf("%d backups, want %d", len(entries), keepBackups)
	}
	if entries[0].Name() != "config-20261101T040000Z.txt" {
		t.Errorf("oldest kept is %s", entries[0].Name())
	}
}

func TestGovernor(t *testing.T) {
	p := newFakePi(t)
	p.write("sys/devices/system/cpu/cpufreq/policy4/scaling_governor", "ondemand\n")
	p.write("sys/devices/system/cpu/cpufreq/policy4/scaling_available_governors", "ondemand performance\n")

	st := p.Status()
	if st.Governor != "ondemand" || !reflect.DeepEqual(st.Governors, []string{"ondemand", "schedutil", "conservative", "performance", "powersave"}) {
		t.Errorf("governor %q of %q", st.Governor, st.Governors)
	}
	for _, bad := range []string{"userspace", "performance\nondemand", "../x", ""} {
		if err := p.SetGovernor(bad); !refused(err) {
			t.Errorf("SetGovernor(%q) = %v, want refused", bad, err)
		}
	}
	if err := p.SetGovernor("performance"); err != nil {
		t.Fatal(err)
	}
	for _, pol := range []string{"policy0", "policy4"} {
		if got := p.read("sys/devices/system/cpu/cpufreq/" + pol + "/scaling_governor"); got != "performance" {
			t.Errorf("%s = %q", pol, got)
		}
	}

	// After a reboot the kernel starts on its default; Boot puts it back.
	p.write("sys/devices/system/cpu/cpufreq/policy0/scaling_governor", "ondemand\n")
	if err := p.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := p.read("sys/devices/system/cpu/cpufreq/policy0/scaling_governor"); got != "performance" {
		t.Errorf("after Boot: %q", got)
	}
}

func TestStatusOnAFreshPi(t *testing.T) {
	p := newFakePi(t)
	st := p.Status()
	if !st.Supported || !st.Watchdog || st.TrialBoot || st.Pending != nil || st.Trial != nil || len(st.Problems) != 0 {
		t.Errorf("Status = %+v", st)
	}
	// Not a Pi at all: still answers, with nothing supported.
	empty := &Host{Root: t.TempDir()}
	st = empty.Status()
	if st.Supported || st.Watchdog || st.Governors == nil {
		t.Errorf("Status off a Pi = %+v", st)
	}
}

func TestCPUCheck(t *testing.T) {
	if err := CPUCheck(context.Background(), 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
}

// A reboot that fails to start keeps the "rebooting" state (systemctl
// may have queued it anyway), and can be tried again. If the Pi then
// boots normally, Boot records that the trial never ran.
func TestRebootTrialFailsToStart(t *testing.T) {
	p := newFakePi(t)
	p.Run = func(context.Context, string, ...string) error { return errors.New("systemctl: access denied") }
	if err := p.Stage(oc); err != nil {
		t.Fatal(err)
	}
	if err := p.RebootTrial(context.Background()); err == nil {
		t.Fatal("RebootTrial = nil, want the systemctl error")
	}
	if err := p.RebootTrial(context.Background()); err == nil || refused(err) {
		t.Fatalf("retry = %v, want the systemctl error again", err)
	}
	if err := p.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.phase() != PhaseFailed {
		t.Errorf("phase %q", p.phase())
	}
}

// Go back, then reboot: the next boot settles it, so the page stops
// asking for a reboot.
func TestDiscardThenReboot(t *testing.T) {
	p := newFakePi(t)
	ctx := context.Background()
	p.Stage(oc)
	p.RebootTrial(ctx)
	p.setTrialBoot(true)
	p.Boot(ctx)
	if err := p.Discard(); err != nil {
		t.Fatal(err)
	}
	p.setTrialBoot(false)
	if err := p.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	if p.phase() != PhaseReverted {
		t.Errorf("phase %q, want reverted", p.phase())
	}
}

// Without the boot unit nothing would check a trial, so it isn't started.
// A trial boot the unit never checked shows as a problem, and can be
// dropped.
func TestTrialNeedsBootCheck(t *testing.T) {
	p := newFakePi(t)
	os.Remove(filepath.Join(p.Root, bootUnitLink))
	p.Stage(oc)
	if err := p.RebootTrial(context.Background()); !refused(err) || len(p.commands()) != 0 {
		t.Errorf("RebootTrial without the boot unit = %v", err)
	}

	p = newFakePi(t)
	p.Stage(oc)
	p.RebootTrial(context.Background())
	p.setTrialBoot(true) // booted, but Boot never ran
	if st := p.Status(); len(st.Problems) == 0 {
		t.Error("Status didn't report the unchecked trial")
	}
	if err := p.Discard(); err != nil || p.phase() != PhaseDiscarded {
		t.Errorf("Discard = %v, phase %q", err, p.phase())
	}
}

// Go back during the check, and the check fails anyway: it still reboots
// back, rather than leave failing settings running.
func TestDiscardDuringFailingCheck(t *testing.T) {
	p := newFakePi(t)
	ctx := context.Background()
	p.Stage(oc)
	p.RebootTrial(ctx)
	p.setTrialBoot(true)
	p.Load = func(context.Context) error {
		if err := p.Discard(); err != nil {
			t.Error(err)
		}
		return errors.New("a CPU computed a wrong hash")
	}
	if err := p.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	if p.phase() != PhaseFailed {
		t.Errorf("phase %q", p.phase())
	}
	want := [][]string{{"systemctl", "reboot", "0 tryboot"}, {"systemctl", "reboot"}}
	if got := p.commands(); !reflect.DeepEqual(got, want) {
		t.Errorf("ran %q, want %q", got, want)
	}
}
