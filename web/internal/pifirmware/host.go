package pifirmware

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The files Host reads and writes, relative to Host.Root. None of them
// ever comes from a request.
const (
	ConfigPath  = "boot/firmware/config.txt"
	TrybootPath = "boot/firmware/tryboot.txt"
	// BackupDir is on the boot partition, so the backups can be read from
	// any computer if the Pi ever needs its SD card pulled.
	BackupDir     = "boot/firmware/admin-backup"
	StateDir      = "var/lib/pi-firmware"
	statePath     = StateDir + "/state.json"
	governorPath  = StateDir + "/governor"
	lockPath      = "run/pi-firmware.lock"
	modelPath     = "proc/device-tree/model"
	trybootFlag   = "proc/device-tree/chosen/bootloader/tryboot"
	watchdogState = "sys/class/watchdog/watchdog0/state"
	cpufreqGlob   = "sys/devices/system/cpu/cpufreq/policy*"
	throttleGlob  = "sys/devices/platform/soc*/*firmware/get_throttled"
)

const (
	keepBackups = 5
	maxFile     = 64 << 10 // config.txt is a few KB
	// underVoltageSinceBoot is get_throttled's bit for an under-voltage
	// since boot.
	underVoltageSinceBoot = 1 << 16
)

// Governors are the CPU governors admin may pick, in the order it shows
// them. Only the ones the kernel has are offered.
var Governors = []string{"ondemand", "schedutil", "conservative", "performance", "powersave"}

// Phase is where the last trial of new settings got to.
type Phase string

// The phases, in the order they usually happen.
const (
	PhaseRebooting Phase = "rebooting" // the Pi is rebooting onto tryboot.txt
	PhaseChecking  Phase = "checking"  // it booted on tryboot.txt; Boot is checking it
	PhasePassed    Phase = "passed"    // the checks passed; it waits for Keep
	PhaseFailed    Phase = "failed"    // a check failed, or the Pi reset: back on config.txt
	PhaseKept      Phase = "kept"      // Keep copied them into config.txt
	PhaseDiscarded Phase = "discarded" // Discard dropped them; a reboot goes back
	PhaseExpired   Phase = "expired"   // it rebooted before Keep: back on config.txt
)

// Trial is the last trial of new settings, saved in StateDir.
type Trial struct {
	Phase    Phase     `json:"phase"`
	Message  string    `json:"message,omitzero"`
	Settings Settings  `json:"settings"`
	At       time.Time `json:"at"`
}

// Status is what admin shows. Nothing in it needs root to read.
type Status struct {
	Model     string `json:"model"`
	Supported bool   `json:"supported"` // a Raspberry Pi 5
	// Saved is the admin block in config.txt: what a normal boot uses.
	Saved Settings `json:"saved"`
	// Pending is the admin block in tryboot.txt, waiting for a trial
	// boot; nil if there's nothing waiting.
	Pending *Settings `json:"pending,omitzero"`
	// TrialBoot is true while the Pi is running on tryboot.txt.
	TrialBoot bool   `json:"trial_boot"`
	Trial     *Trial `json:"trial,omitzero"`
	// Governor is the CPU governor now, and Governors the ones offered.
	Governor  string   `json:"governor,omitzero"`
	Governors []string `json:"governors"`
	// Watchdog is true while something (systemd) serves the hardware
	// watchdog, which a trial boot needs.
	Watchdog bool `json:"watchdog"`
	// Problems are things that need fixing on the Pi by hand, such as a
	// hand-edited admin block.
	Problems []string `json:"problems,omitzero"`
}

// RefusedError is a request Host turned down, with a message for the
// person who asked. Other errors are for the log.
type RefusedError struct{ Msg string }

func (e *RefusedError) Error() string { return e.Msg }

func refuse(format string, a ...any) error {
	return &RefusedError{Msg: fmt.Sprintf(format, a...)}
}

// Host reads and changes one Pi's firmware settings. The changes need
// root; Status doesn't.
type Host struct {
	// Root is where the paths above are found; "" is /. Tests use a
	// folder of synthetic files.
	Root string
	// Run runs a command (systemctl reboot). It must not use a shell.
	Run func(ctx context.Context, name string, args ...string) error
	// Now is the time; time.Now if nil.
	Now func() time.Time

	// Boot's checks on a trial boot. Healthy reports whether admin
	// answers; Boot retries it for HealthWait. Load runs the CPU check
	// (CPUCheck). A nil check is skipped.
	Healthy    func(ctx context.Context) error
	HealthWait time.Duration
	Load       func(ctx context.Context) error
}

func (h *Host) path(rel string) string {
	root := h.Root
	if root == "" {
		root = "/"
	}
	return filepath.Join(root, rel)
}

func (h *Host) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// Status reads the settings and the trial's state. Problems with any one
// part are in Status.Problems rather than an error, so the rest still
// shows.
func (h *Host) Status() Status {
	st := Status{Governors: []string{}}
	problem := func(err error) { st.Problems = append(st.Problems, err.Error()) }

	st.Model = h.model()
	st.Supported = isPi5(st.Model)
	if b, err := readFile(h.path(ConfigPath)); err != nil {
		problem(fmt.Errorf("couldn't read config.txt: %w", err))
	} else if s, _, _, err := ParseBlock(b); err != nil {
		problem(err)
	} else {
		st.Saved = s
	}
	if b, err := readFile(h.path(TrybootPath)); err == nil {
		s, found, _, err := ParseBlock(b)
		switch {
		case err != nil:
			problem(fmt.Errorf("tryboot.txt: %w", err))
		case !found:
			problem(errors.New("tryboot.txt wasn't written by admin, so admin leaves it alone"))
		default:
			st.Pending = &s
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		problem(fmt.Errorf("couldn't read tryboot.txt: %w", err))
	}
	st.TrialBoot = h.trialBoot()
	if t, err := h.readState(); err != nil {
		problem(err)
	} else {
		st.Trial = t
	}
	st.Governor, st.Governors = h.governors()
	st.Watchdog = h.watchdog()
	return st
}

func (h *Host) model() string {
	b, err := readFile(h.path(modelPath))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimRight(string(b), "\x00"))
}

// isPi5 reports whether model is a Raspberry Pi 5 (or a Pi 500, the same
// board in a keyboard). The ranges in Limits are for it only.
func isPi5(model string) bool {
	return strings.HasPrefix(model, "Raspberry Pi 5")
}

func (h *Host) supported() error {
	if m := h.model(); !isPi5(m) {
		if m == "" {
			m = "an unknown board"
		}
		return refuse("These settings are for a Raspberry Pi 5, and this is %s.", m)
	}
	return nil
}

// trialBoot reports whether the firmware booted from tryboot.txt: the
// device tree's tryboot property is a big-endian 1.
func (h *Host) trialBoot() bool {
	b, err := readFile(h.path(trybootFlag))
	return err == nil && len(b) == 4 && binary.BigEndian.Uint32(b) == 1
}

func (h *Host) watchdog() bool {
	b, err := readFile(h.path(watchdogState))
	return err == nil && strings.TrimSpace(string(b)) == "active"
}

// governors returns the current governor and the allowed ones the kernel
// has, from the first cpufreq policy.
func (h *Host) governors() (string, []string) {
	policies, _ := filepath.Glob(h.path(cpufreqGlob))
	if len(policies) == 0 {
		return "", []string{}
	}
	cur, _ := readFile(filepath.Join(policies[0], "scaling_governor"))
	avail, _ := readFile(filepath.Join(policies[0], "scaling_available_governors"))
	have := strings.Fields(string(avail))
	offered := []string{}
	for _, g := range Governors {
		if slices.Contains(have, g) {
			offered = append(offered, g)
		}
	}
	return strings.TrimSpace(string(cur)), offered
}

// busy reports whether a trial is waiting for Keep or still being
// checked, which blocks starting another.
func (h *Host) busy(t *Trial) bool {
	return h.trialBoot() && t != nil && (t.Phase == PhaseChecking || t.Phase == PhasePassed)
}

// Stage writes s to tryboot.txt, ready for RebootTrial. config.txt isn't
// touched.
func (h *Host) Stage(s Settings) error {
	unlock, err := h.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := h.supported(); err != nil {
		return err
	}
	if err := s.Validate(); err != nil {
		return refuse("%s.", err)
	}
	t, err := h.readState()
	if err != nil {
		return err
	}
	if h.busy(t) {
		return refuse("The Pi is trying other settings right now. Keep them or go back first.")
	}
	config, err := h.readConfig()
	if err != nil {
		return err
	}
	out, err := Splice(config, Block(s, true))
	if err != nil {
		return refuse("%s.", err)
	}
	return writeAtomic(h.path(TrybootPath), out, 0o644)
}

// Discard removes the settings waiting in tryboot.txt. During a trial
// boot it means "go back": the next reboot uses config.txt.
func (h *Host) Discard() error {
	unlock, err := h.lock()
	if err != nil {
		return err
	}
	defer unlock()
	t, err := h.readState()
	if err != nil {
		return err
	}
	if err := removeFile(h.path(TrybootPath)); err != nil {
		return err
	}
	if h.busy(t) {
		t.Phase, t.At = PhaseDiscarded, h.now()
		t.Message = "Going back: reboot to return to the saved settings."
		return h.writeState(t)
	}
	return nil
}

// Keep saves the settings a trial boot passed with into config.txt,
// after backing config.txt up, and removes tryboot.txt.
func (h *Host) Keep() error {
	unlock, err := h.lock()
	if err != nil {
		return err
	}
	defer unlock()
	t, err := h.readState()
	if err != nil {
		return err
	}
	if !h.trialBoot() || t == nil || t.Phase != PhasePassed {
		return refuse("Only settings that passed a trial boot can be kept.")
	}
	tb, err := readFile(h.path(TrybootPath))
	if err != nil {
		return refuse("tryboot.txt is gone, so there's nothing to keep.")
	}
	s, found, trial, err := ParseBlock(tb)
	if err != nil || !found || !trial {
		return refuse("tryboot.txt isn't the one admin wrote, so it can't be kept.")
	}
	if !reflect.DeepEqual(s, t.Settings) {
		return refuse("tryboot.txt changed since the trial boot, so it can't be kept.")
	}
	config, err := h.readConfig()
	if err != nil {
		return err
	}
	out, err := Splice(config, Block(s, false))
	if err != nil {
		return refuse("%s.", err)
	}
	if err := h.backup(config); err != nil {
		return fmt.Errorf("backing up config.txt: %w", err)
	}
	if err := writeAtomic(h.path(ConfigPath), out, 0o644); err != nil {
		return err
	}
	if err := removeFile(h.path(TrybootPath)); err != nil {
		return err
	}
	t.Phase, t.At, t.Message = PhaseKept, h.now(), "Saved to config.txt. Every boot uses these settings now."
	return h.writeState(t)
}

// RebootTrial reboots the Pi once onto tryboot.txt. It refuses unless
// tryboot.txt is admin's and the hardware watchdog is running, so a trial
// that hangs resets itself back onto config.txt.
func (h *Host) RebootTrial(ctx context.Context) error {
	unlock, err := h.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := h.supported(); err != nil {
		return err
	}
	t, err := h.readState()
	if err != nil {
		return err
	}
	if h.busy(t) {
		return refuse("The Pi is trying other settings right now. Keep them or go back first.")
	}
	tb, err := readFile(h.path(TrybootPath))
	if errors.Is(err, fs.ErrNotExist) {
		return refuse("No settings are waiting for a trial. Save some first.")
	} else if err != nil {
		return err
	}
	s, found, trial, err := ParseBlock(tb)
	if err != nil || !found || !trial {
		return refuse("tryboot.txt isn't the one admin wrote, so admin won't boot it.")
	}
	if !h.watchdog() {
		return refuse("The hardware watchdog isn't running, so a trial that hangs couldn't reset itself. Set it up first (deploy/pi-firmware/README.md).")
	}
	if err := h.writeState(&Trial{Phase: PhaseRebooting, Settings: s, At: h.now(),
		Message: "Rebooting to try the new settings."}); err != nil {
		return err
	}
	// systemctl passes "0 tryboot" to the reboot system call: partition 0
	// (the default), and the one-shot tryboot flag.
	return h.Run(ctx, "systemctl", "reboot", "0 tryboot")
}

// Reboot reboots the Pi normally, onto config.txt.
func (h *Host) Reboot(ctx context.Context) error {
	unlock, err := h.lock()
	if err != nil {
		return err
	}
	defer unlock()
	return h.Run(ctx, "systemctl", "reboot")
}

// SetGovernor switches every CPU to the governor now, and saves it for
// Boot to apply again after a reboot.
func (h *Host) SetGovernor(name string) error {
	unlock, err := h.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if _, offered := h.governors(); !slices.Contains(offered, name) {
		return refuse("%q isn't a governor admin offers on this Pi.", name)
	}
	if err := h.applyGovernor(name); err != nil {
		return err
	}
	if err := os.MkdirAll(h.path(StateDir), 0o755); err != nil {
		return err
	}
	return writeAtomic(h.path(governorPath), []byte(name+"\n"), 0o644)
}

func (h *Host) applyGovernor(name string) error {
	policies, err := filepath.Glob(h.path(cpufreqGlob))
	if err != nil {
		return err
	}
	if len(policies) == 0 {
		return errors.New("no cpufreq policies")
	}
	for _, p := range policies {
		// sysfs: the file exists, and is written in place.
		f, err := os.OpenFile(filepath.Join(p, "scaling_governor"), os.O_WRONLY|os.O_TRUNC, 0)
		if err != nil {
			return err
		}
		_, err = f.WriteString(name)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("setting %s's governor: %w", filepath.Base(p), err)
		}
	}
	return nil
}

// Boot runs once at every boot, as root. It applies the saved governor,
// then settles the trial: on a trial boot it checks the Pi and reboots
// back to config.txt if a check fails; on a normal boot after a trial
// that never finished, it records that the Pi went back.
func (h *Host) Boot(ctx context.Context) error {
	var errs []error
	if b, err := readFile(h.path(governorPath)); err == nil {
		name := strings.TrimSpace(string(b))
		if _, offered := h.governors(); slices.Contains(offered, name) {
			errs = append(errs, h.applyGovernor(name))
		}
	}
	return errors.Join(append(errs, h.settleTrial(ctx))...)
}

func (h *Host) settleTrial(ctx context.Context) error {
	unlock, err := h.lock()
	if err != nil {
		return err
	}
	t, err := h.readState()
	if err != nil {
		unlock()
		return err
	}
	if !h.trialBoot() {
		defer unlock()
		if t == nil {
			return nil
		}
		switch t.Phase {
		case PhaseRebooting, PhaseChecking:
			t.Phase = PhaseFailed
			t.Message = "The Pi didn't finish the trial boot (it hung, crashed or reset), so it's back on the saved settings."
		case PhasePassed:
			t.Phase = PhaseExpired
			t.Message = "The Pi rebooted before the trial settings were kept, so it's back on the saved settings."
		default:
			return nil
		}
		t.At = h.now()
		return errors.Join(removeFile(h.path(TrybootPath)), h.writeState(t))
	}

	if t == nil || t.Phase != PhaseRebooting {
		// A trial boot admin didn't start: leave it to whoever did.
		unlock()
		return nil
	}
	t.Phase, t.At, t.Message = PhaseChecking, h.now(), "Booted with the new settings. Checking the Pi…"
	err = h.writeState(t)
	unlock()
	if err != nil {
		return err
	}

	checkErr := h.check(ctx)

	unlock, err = h.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if t, err = h.readState(); err != nil {
		return err
	}
	if t == nil || t.Phase != PhaseChecking {
		return nil // discarded while the check ran
	}
	t.At = h.now()
	if checkErr == nil {
		t.Phase = PhasePassed
		t.Message = "The new settings passed their checks. Keep them, or go back."
		return h.writeState(t)
	}
	t.Phase = PhaseFailed
	t.Message = fmt.Sprintf("The new settings failed a check (%v), so the Pi rebooted back to the saved settings.", checkErr)
	if err := errors.Join(removeFile(h.path(TrybootPath)), h.writeState(t)); err != nil {
		return err
	}
	return h.Run(ctx, "systemctl", "reboot")
}

// check is a trial boot's health check: admin answers, there's been no
// under-voltage, and the CPU gets through a full load.
func (h *Host) check(ctx context.Context) error {
	if h.Healthy != nil {
		ctx, cancel := context.WithTimeout(ctx, h.HealthWait)
		err := h.Healthy(ctx)
		for err != nil {
			select {
			case <-ctx.Done():
				cancel()
				return fmt.Errorf("admin didn't answer within %s: %w", h.HealthWait, err)
			case <-time.After(time.Second):
			}
			err = h.Healthy(ctx)
		}
		cancel()
	}
	if paths, _ := filepath.Glob(h.path(throttleGlob)); len(paths) > 0 {
		b, err := readFile(paths[0])
		if err != nil {
			return fmt.Errorf("reading get_throttled: %w", err)
		}
		v, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(string(b)), "0x"), 16, 32)
		if err != nil {
			return fmt.Errorf("reading get_throttled: %w", err)
		}
		if v&underVoltageSinceBoot != 0 {
			return errors.New("under-voltage since boot")
		}
	}
	if h.Load != nil {
		if err := h.Load(ctx); err != nil {
			return fmt.Errorf("CPU check: %w", err)
		}
	}
	return nil
}

// readConfig reads config.txt, refusing one whose admin block isn't
// admin's, so a hand edit is never silently replaced.
func (h *Host) readConfig() ([]byte, error) {
	config, err := readFile(h.path(ConfigPath))
	if err != nil {
		return nil, err
	}
	if _, _, _, err := ParseBlock(config); err != nil {
		return nil, refuse("%s.", strings.TrimSuffix(err.Error(), "."))
	}
	return config, nil
}

// backup copies config to BackupDir, keeping the newest few.
func (h *Host) backup(config []byte) error {
	dir := h.path(BackupDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if fi, err := os.Lstat(dir); err != nil {
		return err
	} else if !fi.IsDir() {
		return fmt.Errorf("%s isn't a folder", dir)
	}
	name := "config-" + h.now().UTC().Format("20060102T150405Z") + ".txt"
	if err := writeAtomic(filepath.Join(dir, name), config, 0o644); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var old []string
	for _, e := range entries {
		if n := e.Name(); e.Type().IsRegular() && strings.HasPrefix(n, "config-") && strings.HasSuffix(n, ".txt") {
			old = append(old, n)
		}
	}
	slices.Sort(old)
	for _, n := range old[:max(0, len(old)-keepBackups)] {
		if err := os.Remove(filepath.Join(dir, n)); err != nil {
			return err
		}
	}
	return nil
}

func (h *Host) readState() (*Trial, error) {
	b, err := readFile(h.path(statePath))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var t Trial
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("reading %s: %w", statePath, err)
	}
	return &t, nil
}

func (h *Host) writeState(t *Trial) error {
	if err := os.MkdirAll(h.path(StateDir), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(h.path(statePath), append(b, '\n'), 0o644)
}

// lock serializes every change, across processes: admin's requests and
// the boot check.
func (h *Host) lock() (func(), error) {
	p := h.path(lockPath)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

// readFile reads a small regular file without following a symlink at
// path.
func readFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s isn't a regular file", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxFile {
		return nil, fmt.Errorf("%s is too big", path)
	}
	return b, nil
}

// writeAtomic replaces path with data: a temporary file in the same
// folder, synced, then renamed over it, then the folder synced. On the
// FAT boot partition a power cut leaves either the old file or the new
// one, never half of each.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if fi, err := os.Lstat(path); err == nil && !fi.Mode().IsRegular() {
		return fmt.Errorf("%s isn't a regular file", path)
	}
	tmp := filepath.Join(dir, ".pi-firmware.tmp")
	if err := removeFile(tmp); err != nil {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(dir)
}

// removeFile removes path if it's there, and syncs its folder.
func removeFile(path string) error {
	err := os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}
