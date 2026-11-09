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

var testNow = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

// fakePi is a folder laid out like a Pi 5's /boot/firmware, /proc, /sys,
// /etc and /var, with synthetic files, and a record of the commands run.
type fakePi struct {
	*Host
	t        *testing.T
	mu       sync.Mutex
	runs     [][]string
	serverOn bool
	runErr   error
}

func newFakePi(t *testing.T) *fakePi {
	t.Helper()
	root := t.TempDir()
	p := &fakePi{t: t}
	p.Host = &Host{
		Root: root,
		Now:  func() time.Time { return testNow },
		Run: func(_ context.Context, name string, args ...string) error {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.runs = append(p.runs, append([]string{name}, args...))
			if len(args) >= 4 && args[3] == "stop" {
				p.serverOn = false
			}
			return p.runErr
		},
		Throttled: func(context.Context) (uint32, error) { return 0, nil },
		Load:      func(context.Context) error { time.Sleep(20 * time.Millisecond); return nil },
		PollEvery: 2 * time.Millisecond,
		ServerUp: func() bool {
			p.mu.Lock()
			defer p.mu.Unlock()
			return p.serverOn
		},
		User: "pi",
	}
	p.write(modelPath, "Raspberry Pi 5 Model B Rev 1.0\x00")
	p.write(ConfigPath, stock)
	p.write(watchdogState, "active\n")
	p.write(bootUnitLink, "")
	pol := "sys/devices/system/cpu/cpufreq/policy0/"
	p.write(pol+"scaling_governor", "ondemand\n")
	p.write(pol+"scaling_available_governors", "conservative ondemand userspace powersave performance schedutil \n")
	p.write(pol+"scaling_cur_freq", "3000000\n")
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

func (p *fakePi) setServer(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.serverOn = on
}

func (p *fakePi) commands() [][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.runs)
}

// reboots returns the reboot commands run.
func (p *fakePi) reboots() [][]string {
	var out [][]string
	for _, c := range p.commands() {
		if len(c) >= 2 && c[1] == "reboot" {
			out = append(out, c)
		}
	}
	return out
}

func (p *fakePi) state() *Trial {
	p.t.Helper()
	tr, err := p.readState()
	if err != nil {
		p.t.Fatal(err)
	}
	return tr
}

func (p *fakePi) phase() Phase {
	p.t.Helper()
	if tr := p.state(); tr != nil {
		return tr.Phase
	}
	return ""
}

// startTrial stages s, reboots onto it and boots the trial: Boot runs in
// the background, since a trial boot's Boot waits for Keep. done gets
// Boot's error.
func (p *fakePi) startTrial(s Settings) (done chan error) {
	p.t.Helper()
	if err := p.Stage(s); err != nil {
		p.t.Fatal(err)
	}
	if err := p.RebootTrial(context.Background()); err != nil {
		p.t.Fatal(err)
	}
	p.setTrialBoot(true)
	done = make(chan error, 1)
	go func() { done <- p.Boot(context.Background()) }()
	return done
}

// waitPhase waits for the trial to reach one of phases.
func (p *fakePi) waitPhase(phases ...Phase) {
	p.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !slices.Contains(phases, p.phase()) {
		if time.Now().After(deadline) {
			p.t.Fatalf("phase %q, want one of %q", p.phase(), phases)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func wait(t *testing.T, done chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Boot didn't return")
		return nil
	}
}

func refused(err error) bool {
	var r *RefusedError
	return errors.As(err, &r)
}

var oc = Settings{ArmFreq: new(2600), OverVoltageDelta: new(25000)}

// The whole happy path: stage, reboot onto tryboot.txt, pass the check,
// keep. config.txt changes only at Keep, after a backup, and gets the
// watchdog and force_turbo=0 lines too.
func TestTrialKept(t *testing.T) {
	p := newFakePi(t)
	if err := p.Stage(oc); err != nil {
		t.Fatal(err)
	}
	if p.read(ConfigPath) != stock {
		t.Fatal("Stage changed config.txt")
	}
	if st := p.Status(); st.Pending == nil || !reflect.DeepEqual(*st.Pending, oc) || !st.Saved.Empty() {
		t.Fatalf("after Stage: pending %+v, saved %+v", st.Pending, st.Saved)
	}
	if err := p.RebootTrial(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := p.commands(); !reflect.DeepEqual(got, [][]string{{"systemctl", "reboot", "0 tryboot"}}) {
		t.Fatalf("ran %q", got)
	}

	p.setTrialBoot(true)
	done := make(chan error, 1)
	go func() { done <- p.Boot(context.Background()) }()
	p.waitPhase(PhasePassed)

	// Nothing else may be staged, dropped or booted until it's kept.
	if err := p.Stage(Settings{ArmFreq: new(2800)}); !refused(err) {
		t.Errorf("Stage during a trial = %v, want refused", err)
	}
	if err := p.RebootTrial(context.Background()); !refused(err) {
		t.Errorf("RebootTrial during a trial = %v, want refused", err)
	}

	if err := p.Keep(); err != nil {
		t.Fatal(err)
	}
	if err := wait(t, done); err != nil {
		t.Fatalf("Boot = %v", err)
	}
	if p.exists(TrybootPath) {
		t.Error("Keep left tryboot.txt")
	}
	config := p.read(ConfigPath)
	if !strings.HasPrefix(config, stock) || !strings.Contains(config, "\nkernel_watchdog_timeout=60\n") || !strings.Contains(config, "\nforce_turbo=0\n") {
		t.Errorf("config.txt after Keep:\n%s", config)
	}
	if s, _, _ := ParseBlock([]byte(config)); !reflect.DeepEqual(s, oc) {
		t.Errorf("config.txt has %+v, want %+v", s, oc)
	}
	tr := p.state()
	if tr.Phase != PhaseKept || tr.Backup != "config-20261107T120000Z.txt" || tr.Resets != 0 {
		t.Errorf("state after Keep = %+v", tr)
	}
	if got := p.read(filepath.Join(BackupDir, tr.Backup)); got != stock {
		t.Errorf("backup = %q, want the old config.txt", got)
	}
	if st := p.Status(); !st.ProbationUntil.Equal(testNow.Add(Probation)) {
		t.Errorf("probation until %v", st.ProbationUntil)
	}
	if len(p.reboots()) != 1 {
		t.Errorf("rebooted %q", p.reboots())
	}
}

// A check that fails on the trial boot drops tryboot.txt and reboots
// back onto config.txt, which is never touched.
func TestTrialFailsCheck(t *testing.T) {
	// afterFirst returns v from every get_throttled read but the first,
	// so the trouble shows during the load.
	afterFirst := func(v uint32) func(context.Context) (uint32, error) {
		var mu sync.Mutex
		calls := 0
		return func(context.Context) (uint32, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if calls > 1 {
				return v, nil
			}
			return 0, nil
		}
	}
	for name, setup := range map[string]func(p *fakePi){
		"under-voltage before": func(p *fakePi) {
			p.Throttled = func(context.Context) (uint32, error) { return 0x50000, nil }
		},
		"throttled during the load": func(p *fakePi) { p.Throttled = afterFirst(throttled) },
		"capped recently":           func(p *fakePi) { p.Throttled = afterFirst(freqCapped << sinceShift) },
		"no throttle reading": func(p *fakePi) {
			p.Throttled = func(context.Context) (uint32, error) { return 0, errors.New("vcgencmd: not found") }
		},
		"wrong hash": func(p *fakePi) {
			p.Load = func(context.Context) error { return errors.New("a CPU computed a wrong hash") }
		},
		"clock too low": func(p *fakePi) {
			p.write("sys/devices/system/cpu/cpufreq/policy0/scaling_cur_freq", "2400000\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := newFakePi(t)
			setup(p)
			done := p.startTrial(oc)
			if err := wait(t, done); err != nil {
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
			if got := p.reboots(); !reflect.DeepEqual(got, want) {
				t.Errorf("rebooted %q, want %q", got, want)
			}
			if err := p.Keep(); !refused(err) {
				t.Errorf("Keep after a failed trial = %v, want refused", err)
			}
		})
	}
}

// The check runs the load under the performance governor, and puts the
// saved one back after.
func TestCheckUsesPerformance(t *testing.T) {
	p := newFakePi(t)
	gov := "sys/devices/system/cpu/cpufreq/policy0/scaling_governor"
	var mu sync.Mutex
	var during string
	p.Load = func(context.Context) error {
		b, _ := os.ReadFile(filepath.Join(p.Root, gov))
		mu.Lock()
		during = string(b)
		mu.Unlock()
		return nil
	}
	done := p.startTrial(oc)
	p.waitPhase(PhasePassed)
	mu.Lock()
	defer mu.Unlock()
	if during != "performance" || p.read(gov) != "ondemand" {
		t.Errorf("governor during the load %q, after %q", during, p.read(gov))
	}
	p.Keep()
	wait(t, done)
}

// Paper never runs on settings that haven't been kept: the trial boot
// stops minecraft.service whenever it's up, and stops holding it after
// Keep.
func TestTrialHoldsServerOff(t *testing.T) {
	p := newFakePi(t)
	done := p.startTrial(oc)
	p.setServer(true) // the user manager starts it at boot
	p.waitPhase(PhasePassed)
	deadline := time.Now().Add(5 * time.Second)
	for p.ServerUp() {
		if time.Now().After(deadline) {
			t.Fatal("the server was never stopped")
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop := []string{"systemctl", "--user", "-M", "pi@", "stop", "minecraft.service"}
	if !slices.ContainsFunc(p.commands(), func(c []string) bool { return slices.Equal(c, stop) }) {
		t.Errorf("ran %q, want %q", p.commands(), stop)
	}
	if err := p.Keep(); err != nil {
		t.Fatal(err)
	}
	wait(t, done)
	p.setServer(true)
	time.Sleep(20 * time.Millisecond)
	if !p.ServerUp() {
		t.Error("the server was stopped after Keep")
	}
}

// A trial that passed but isn't kept goes back by itself.
func TestTrialExpiresWithoutKeep(t *testing.T) {
	p := newFakePi(t)
	p.KeepWindow = 30 * time.Millisecond
	done := p.startTrial(oc)
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
	if p.phase() != PhaseExpired || p.exists(TrybootPath) {
		t.Errorf("phase %q, tryboot.txt %v", p.phase(), p.exists(TrybootPath))
	}
	if got := p.reboots(); len(got) != 2 || !slices.Equal(got[1], []string{"systemctl", "reboot"}) {
		t.Errorf("rebooted %q", got)
	}
}

// The firmware's own fallback: the trial hung or reset, or the Pi
// rebooted before Keep, so the next boot is a normal one on config.txt.
// Boot records that and clears up.
func TestTrialSettlesOnNormalBoot(t *testing.T) {
	for phase, want := range map[Phase]Phase{
		PhaseRebooting: PhaseFailed,
		PhaseChecking:  PhaseFailed,
		PhasePassed:    PhaseExpired,
		PhaseDiscarded: PhaseReverted,
		PhaseKept:      PhaseKept,
		PhaseFailed:    PhaseFailed,
	} {
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
		if p.phase() != want || p.read(ConfigPath) != stock {
			t.Errorf("from %s: phase %q, want %q", phase, p.phase(), want)
		}
		if phase != PhaseKept && phase != PhaseFailed && p.exists(TrybootPath) {
			t.Errorf("from %s: tryboot.txt is still there", phase)
		}
		if len(p.commands()) != 0 {
			t.Errorf("from %s: ran %q", phase, p.commands())
		}
	}
}

// Keep wrote config.txt, then the power went before the state said so:
// the next boot sees the settings in config.txt and puts them on
// probation, rather than calling them not kept.
func TestPowerCutDuringKeep(t *testing.T) {
	p := newFakePi(t)
	p.write(ConfigPath, string(must(Splice([]byte(stock), Block(oc)))))
	p.write(filepath.Join(BackupDir, "config-20261107T115900Z.txt"), stock)
	if err := p.writeState(&Trial{Phase: PhasePassed, Settings: oc, Backup: "config-20261107T115900Z.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tr := p.state(); tr.Phase != PhaseKept || tr.Backup != "config-20261107T115900Z.txt" {
		t.Errorf("state = %+v", tr)
	}
}

// Kept settings that reset the Pi uncleanly twice within the probation
// put the old config.txt back and reboot onto it. A clean shutdown
// doesn't count, and neither does a reset after the probation.
func TestProbation(t *testing.T) {
	ctx := context.Background()
	kept := func(p *fakePi, at time.Time) {
		p.write(ConfigPath, string(must(Splice([]byte(stock), Block(oc)))))
		p.write(filepath.Join(BackupDir, "config-old.txt"), stock)
		if err := p.writeState(&Trial{Phase: PhaseKept, Settings: oc, At: at, Backup: "config-old.txt"}); err != nil {
			t.Fatal(err)
		}
	}

	p := newFakePi(t)
	kept(p, testNow.Add(-24*time.Hour))
	// First boot after Keep: no marker yet, so it's clean.
	if err := p.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	// A clean shutdown, then a boot: still clean.
	if err := p.CleanStop(); err != nil {
		t.Fatal(err)
	}
	p.Boot(ctx)
	if tr := p.state(); tr.Resets != 0 {
		t.Fatalf("clean boots counted: %+v", tr)
	}
	// Two unclean resets (no CleanStop in between).
	p.Boot(ctx)
	if tr := p.state(); tr.Resets != 1 || tr.Phase != PhaseKept || len(p.reboots()) != 0 {
		t.Fatalf("after one reset: %+v, rebooted %q", tr, p.reboots())
	}
	p.Boot(ctx)
	if tr := p.state(); tr.Phase != PhaseRolledBack {
		t.Fatalf("after two resets: %+v", tr)
	}
	if p.read(ConfigPath) != stock {
		t.Error("the old config.txt wasn't put back")
	}
	if got := p.reboots(); !reflect.DeepEqual(got, [][]string{{"systemctl", "reboot"}}) {
		t.Errorf("rebooted %q", got)
	}

	// After the probation, resets don't count.
	p = newFakePi(t)
	kept(p, testNow.Add(-Probation-time.Hour))
	p.Boot(ctx)
	p.Boot(ctx)
	p.Boot(ctx)
	if tr := p.state(); tr.Phase != PhaseKept || tr.Resets != 0 {
		t.Errorf("after the probation: %+v", tr)
	}

	// A backup name from the state never leaves the backup folder.
	p = newFakePi(t)
	kept(p, testNow.Add(-time.Hour))
	tr := p.state()
	tr.Backup = "../config.txt"
	p.writeState(tr)
	p.Boot(ctx)
	p.Boot(ctx)
	p.Boot(ctx)
	if tr := p.state(); tr.Phase != PhaseKept || len(p.reboots()) != 0 {
		t.Errorf("with a bad backup name: %+v, rebooted %q", tr, p.reboots())
	}
}

func TestGoBack(t *testing.T) {
	p := newFakePi(t)
	done := p.startTrial(oc)
	p.waitPhase(PhasePassed)
	if err := p.Discard(); err != nil {
		t.Fatal(err)
	}
	if p.exists(TrybootPath) || p.phase() != PhaseDiscarded {
		t.Errorf("tryboot.txt %v, phase %q", p.exists(TrybootPath), p.phase())
	}
	// Going back: nothing else until the reboot finishes it.
	if err := p.Keep(); !refused(err) {
		t.Errorf("Keep after Go back = %v, want refused", err)
	}
	if err := p.Stage(Settings{TempLimit: new(80)}); !refused(err) {
		t.Errorf("Stage after Go back = %v, want refused", err)
	}
	if err := p.Discard(); !refused(err) {
		t.Errorf("Discard after Go back = %v, want refused", err)
	}
	// Boot keeps holding the server off until the reboot.
	select {
	case err := <-done:
		t.Fatalf("Boot returned %v while going back", err)
	case <-time.After(20 * time.Millisecond):
	}
	// The reboot, and the next boot settles it.
	p.setTrialBoot(false)
	if err := p.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.phase() != PhaseReverted {
		t.Errorf("phase %q, want reverted", p.phase())
	}
	if err := p.Stage(Settings{TempLimit: new(80)}); err != nil {
		t.Errorf("Stage after going back: %v", err)
	}
	// The first Boot is still waiting: it ends once the state isn't
	// passed or discarded.
	wait(t, done)
}

// Go back during the check, and the check fails anyway: it still reboots
// back, rather than leave failing settings running.
func TestGoBackDuringFailingCheck(t *testing.T) {
	p := newFakePi(t)
	p.Load = func(context.Context) error {
		if err := p.Discard(); err != nil {
			t.Error(err)
		}
		return errors.New("a CPU computed a wrong hash")
	}
	done := p.startTrial(oc)
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
	if p.phase() != PhaseFailed || len(p.reboots()) != 2 {
		t.Errorf("phase %q, rebooted %q", p.phase(), p.reboots())
	}
}

// Between RebootTrial and the reboot, nothing may change tryboot.txt;
// RebootTrial itself may be tried again. A reboot that never happened
// is settled as failed by the next normal boot.
func TestRebootPending(t *testing.T) {
	p := newFakePi(t)
	p.runErr = errors.New("systemctl: access denied")
	if err := p.Stage(oc); err != nil {
		t.Fatal(err)
	}
	if err := p.RebootTrial(context.Background()); err == nil || refused(err) {
		t.Fatalf("RebootTrial = %v, want the systemctl error", err)
	}
	if err := p.Stage(Settings{TempLimit: new(80)}); !refused(err) {
		t.Errorf("Stage while a reboot is pending = %v", err)
	}
	if err := p.Discard(); !refused(err) {
		t.Errorf("Discard while a reboot is pending = %v", err)
	}
	if err := p.RebootTrial(context.Background()); err == nil || refused(err) {
		t.Errorf("retry = %v, want the systemctl error again", err)
	}
	if err := p.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.phase() != PhaseFailed {
		t.Errorf("phase %q", p.phase())
	}
}

// A trial boot the boot unit never checked shows as a problem, and Go
// back clears it.
func TestUncheckedTrial(t *testing.T) {
	p := newFakePi(t)
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

func TestRefusals(t *testing.T) {
	ctx := context.Background()

	p := newFakePi(t)
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

	// The server must be stopped, checked by the helper too.
	p = newFakePi(t)
	p.Stage(oc)
	p.setServer(true)
	if err := p.RebootTrial(ctx); !refused(err) {
		t.Errorf("RebootTrial with the server up = %v, want refused", err)
	}
	if err := p.Reboot(ctx); !refused(err) {
		t.Errorf("Reboot with the server up = %v, want refused", err)
	}
	if len(p.commands()) != 0 {
		t.Errorf("ran %q", p.commands())
	}

	// No watchdog or no boot unit: nothing is staged or tried.
	for name, rel := range map[string]string{"watchdog": watchdogState, "boot unit": bootUnitLink} {
		p = newFakePi(t)
		os.Remove(filepath.Join(p.Root, rel))
		if err := p.Stage(oc); !refused(err) || p.exists(TrybootPath) {
			t.Errorf("Stage without the %s = %v", name, err)
		}
		p.write(TrybootPath, string(must(Splice([]byte(stock), Block(oc)))))
		if err := p.RebootTrial(ctx); !refused(err) || len(p.commands()) != 0 {
			t.Errorf("RebootTrial without the %s = %v", name, err)
		}
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

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
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
		if _, err := p.backup([]byte{byte('a' + i)}); err != nil {
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
	if !st.Supported || !st.Watchdog || !st.BootCheck || st.TrialBoot || st.Pending != nil || st.Trial != nil || len(st.Problems) != 0 {
		t.Errorf("Status = %+v", st)
	}
	// Kept settings arm the watchdog: without it, that's a problem.
	p.write(ConfigPath, string(must(Splice([]byte(stock), Block(oc)))))
	p.write(watchdogState, "inactive\n")
	if st := p.Status(); len(st.Problems) != 1 {
		t.Errorf("problems = %q", st.Problems)
	}
	// Not a Pi at all: still answers, with nothing supported.
	empty := &Host{Root: t.TempDir()}
	st = empty.Status()
	if st.Supported || st.Watchdog || st.Governors == nil {
		t.Errorf("Status off a Pi = %+v", st)
	}
}

func TestParseThrottled(t *testing.T) {
	for in, want := range map[string]uint32{"throttled=0x0\n": 0, "throttled=0x50005": 0x50005} {
		if got, err := ParseThrottled(in); err != nil || got != want {
			t.Errorf("ParseThrottled(%q) = %#x, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "0x5", "throttled=zz", "error=1"} {
		if _, err := ParseThrottled(bad); err == nil {
			t.Errorf("ParseThrottled(%q) = nil error", bad)
		}
	}
}

func TestCPUCheck(t *testing.T) {
	if err := CPUCheck(context.Background(), 50*time.Millisecond, 1<<20); err != nil {
		t.Fatal(err)
	}
}

func TestStopServerNeedsAValidUser(t *testing.T) {
	p := newFakePi(t)
	for _, u := range []string{"", "root@host", "-H", "a b"} {
		p.User = u
		if err := p.stopServer(context.Background()); err == nil {
			t.Errorf("stopServer as %q = nil", u)
		}
	}
	if len(p.commands()) != 0 {
		t.Errorf("ran %q", p.commands())
	}
}

// If the server can't be kept off, the trial fails rather than let Paper
// run on settings that haven't passed.
func TestTrialFailsIfServerWontStop(t *testing.T) {
	p := newFakePi(t)
	run := p.Run
	p.Run = func(ctx context.Context, name string, args ...string) error {
		if len(args) >= 4 && args[3] == "stop" {
			return errors.New("Failed to connect to bus")
		}
		return run(ctx, name, args...)
	}
	done := p.startTrial(oc)
	p.setServer(true)
	// Boot reports the failed stop, and reboots back anyway.
	if err := wait(t, done); err == nil || !strings.Contains(err.Error(), "bus") {
		t.Errorf("Boot = %v, want the stop error", err)
	}
	if tr := p.state(); tr.Phase != PhaseFailed || !strings.Contains(tr.Message, "kept running") {
		t.Errorf("state = %+v", tr)
	}
	if got := p.reboots(); len(got) != 2 {
		t.Errorf("rebooted %q", got)
	}
}
