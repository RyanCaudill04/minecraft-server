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
	"regexp"
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
	BackupDir    = "boot/firmware/admin-backup"
	StateDir     = "var/lib/pi-firmware"
	statePath    = StateDir + "/state.json"
	governorPath = StateDir + "/governor"
	// markerPath is there while the Pi runs: Boot writes it and CleanStop,
	// at a clean shutdown, removes it. Finding it at boot means the last
	// boot ended in a crash, a watchdog reset or a power cut.
	markerPath    = StateDir + "/running"
	lockPath      = "run/pi-firmware.lock"
	modelPath     = "proc/device-tree/model"
	trybootFlag   = "proc/device-tree/chosen/bootloader/tryboot"
	watchdogState = "sys/class/watchdog/watchdog0/state"
	// bootUnitLink exists while pi-firmware-boot.service is enabled.
	bootUnitLink = "etc/systemd/system/multi-user.target.wants/pi-firmware-boot.service"
	cpufreqGlob  = "sys/devices/system/cpu/cpufreq/policy*"
)

const (
	keepBackups = 5
	maxFile     = 64 << 10 // config.txt is a few KB

	// Probation is how long after Keep the Pi is watched for unclean
	// resets, and ProbationResets how many put the old config.txt back.
	Probation       = 14 * 24 * time.Hour
	ProbationResets = 2
	// KeepWindow is how long a trial that passed waits for Keep before it
	// goes back by itself.
	KeepWindow = 30 * time.Minute
)

// get_throttled's bits: now in the low ones, since boot (or, with the
// kernel's hwmon driver polling, the last couple of seconds) 16 higher.
const (
	underVoltage = 1 << 0
	freqCapped   = 1 << 1
	throttled    = 1 << 2
	sinceShift   = 16
	// badBits fail a trial: under-voltage, a capped clock or throttling,
	// now or recently.
	badBits = (underVoltage | freqCapped | throttled) | (underVoltage|freqCapped|throttled)<<sinceShift
)

// Governors are the CPU governors admin may pick, in the order it shows
// them. Only the ones the kernel has are offered.
var Governors = []string{"ondemand", "schedutil", "conservative", "performance", "powersave"}

// Phase is where the last trial of new settings got to.
type Phase string

// The phases, in the order they usually happen.
const (
	PhaseRebooting  Phase = "rebooting"   // the Pi is rebooting onto tryboot.txt
	PhaseChecking   Phase = "checking"    // it booted on tryboot.txt; Boot is checking it
	PhasePassed     Phase = "passed"      // the checks passed; it waits for Keep
	PhaseFailed     Phase = "failed"      // a check failed, or the Pi reset: back on config.txt
	PhaseKept       Phase = "kept"        // Keep copied them into config.txt; on probation
	PhaseDiscarded  Phase = "discarded"   // Go back during the trial; a reboot goes back
	PhaseExpired    Phase = "expired"     // not kept in time, or rebooted before Keep: back on config.txt
	PhaseReverted   Phase = "reverted"    // rebooted after Go back: back on config.txt
	PhaseRolledBack Phase = "rolled_back" // kept, then reset too often: old config.txt restored
)

// Trial is the last trial of new settings, saved in StateDir.
type Trial struct {
	Phase    Phase     `json:"phase"`
	Message  string    `json:"message,omitzero"`
	Settings Settings  `json:"settings"`
	At       time.Time `json:"at"`
	// Backup is the config.txt backup Keep made, which probation puts
	// back if the kept settings reset the Pi.
	Backup string `json:"backup,omitzero"`
	// Resets counts unclean boots during probation.
	Resets int `json:"resets,omitzero"`
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
	// ProbationUntil is when kept settings stop being watched for resets.
	ProbationUntil time.Time `json:"probation_until,omitzero"`
	// Governor is the CPU governor now, and Governors the ones offered.
	Governor  string   `json:"governor,omitzero"`
	Governors []string `json:"governors"`
	// Watchdog is true while something (systemd) serves the hardware
	// watchdog, which every admin block arms.
	Watchdog bool `json:"watchdog"`
	// BootCheck is true while pi-firmware-boot.service is enabled, which
	// checks a trial boot and watches kept settings.
	BootCheck bool `json:"boot_check"`
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
	// Run runs a command (systemctl). It must not use a shell.
	Run func(ctx context.Context, name string, args ...string) error
	// Now is the time; time.Now if nil.
	Now func() time.Time

	// Throttled returns the firmware's get_throttled bits (vcgencmd).
	// Load runs the CPU check (CPUCheck). The trial check polls Throttled
	// and the clock every PollEvery (default a second) while Load runs.
	Throttled func(ctx context.Context) (uint32, error)
	Load      func(ctx context.Context) error
	PollEvery time.Duration
	// KeepWindow overrides the package's KeepWindow (tests).
	KeepWindow time.Duration

	// ServerUp reports whether the Minecraft server (LazyMC) is running.
	// Reboot and RebootTrial refuse while it is, and a trial boot stops
	// it, as User, so Paper never runs on settings that haven't passed.
	// nil means it's never up.
	ServerUp func() bool
	// User is the Unix user whose minecraft.service a trial boot stops.
	User string
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

func (h *Host) serverUp() bool { return h.ServerUp != nil && h.ServerUp() }

// validUser matches a Unix user name, before it goes into "-M user@".
var validUser = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// stopServer stops the user's minecraft.service, so Paper saves first.
func (h *Host) stopServer(ctx context.Context) error {
	if !validUser.MatchString(h.User) {
		return fmt.Errorf("no valid user to stop minecraft.service as (PI_FIRMWARE_USER is %q)", h.User)
	}
	return h.Run(ctx, "systemctl", "--user", "-M", h.User+"@", "stop", "minecraft.service")
}

// Status reads the settings and the trial's state. Problems with any one
// part are in Status.Problems rather than an error, so the rest still
// shows.
func (h *Host) Status() Status {
	st := Status{Governors: []string{}}
	problem := func(err error) { st.Problems = append(st.Problems, err.Error()) }

	st.Model = h.model()
	st.Supported = isPi5(st.Model)
	savedBlock := false
	if b, err := readFile(h.path(ConfigPath)); err != nil {
		problem(fmt.Errorf("couldn't read config.txt: %w", err))
	} else if s, found, err := ParseBlock(b); err != nil {
		problem(err)
	} else {
		st.Saved, savedBlock = s, found
	}
	if b, err := readFile(h.path(TrybootPath)); err == nil {
		s, found, err := ParseBlock(b)
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
	st.TrialBoot = h.TrialBoot()
	if t, err := h.readState(); err != nil {
		problem(err)
	} else {
		st.Trial = t
	}
	if t := st.Trial; t != nil && t.Phase == PhaseKept && h.now().Before(t.At.Add(Probation)) {
		st.ProbationUntil = t.At.Add(Probation)
	}
	st.Governor, st.Governors = h.governors()
	st.Watchdog = h.watchdog()
	st.BootCheck = h.bootCheck()
	if st.TrialBoot && st.Trial != nil && st.Trial.Phase == PhaseRebooting {
		problem(errors.New("the Pi booted the trial settings, but pi-firmware-boot.service hasn't checked them: see its log, or go back"))
	}
	if savedBlock && !st.Watchdog {
		problem(errors.New("config.txt arms the hardware watchdog, but systemd isn't serving it, so the Pi may reset soon after every boot: reinstall watchdog.conf (deploy/pi-firmware/README.md)"))
	}
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

// TrialBoot reports whether the firmware booted from tryboot.txt: the
// device tree's tryboot property is a big-endian 1.
func (h *Host) TrialBoot() bool {
	b, err := readFile(h.path(trybootFlag))
	return err == nil && len(b) == 4 && binary.BigEndian.Uint32(b) == 1
}

func (h *Host) bootCheck() bool {
	_, err := os.Lstat(h.path(bootUnitLink))
	return err == nil
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

// curKHz is the first cpufreq policy's current clock, in kHz.
func (h *Host) curKHz() (int, error) {
	policies, _ := filepath.Glob(h.path(cpufreqGlob))
	if len(policies) == 0 {
		return 0, errors.New("no cpufreq policies")
	}
	b, err := readFile(filepath.Join(policies[0], "scaling_cur_freq"))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
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

// ParseThrottled reads vcgencmd get_throttled's output, such as
// "throttled=0x50005".
func ParseThrottled(out string) (uint32, error) {
	s, ok := strings.CutPrefix(strings.TrimSpace(out), "throttled=")
	if !ok {
		return 0, fmt.Errorf("unexpected get_throttled output %q", out)
	}
	v, err := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 32)
	if err != nil {
		return 0, fmt.Errorf("unexpected get_throttled output %q", out)
	}
	return uint32(v), nil
}

// describeThrottle says which of get_throttled's bad bits are set.
func describeThrottle(v uint32) string {
	var parts []string
	if v&(underVoltage|underVoltage<<sinceShift) != 0 {
		parts = append(parts, "under-voltage")
	}
	if v&(freqCapped|freqCapped<<sinceShift) != 0 {
		parts = append(parts, "clock capped")
	}
	if v&(throttled|throttled<<sinceShift) != 0 {
		parts = append(parts, "throttled")
	}
	return strings.Join(parts, ", ")
}

// readConfig reads config.txt, refusing one whose admin block isn't
// admin's, so a hand edit is never silently replaced.
func (h *Host) readConfig() ([]byte, error) {
	config, err := readFile(h.path(ConfigPath))
	if err != nil {
		return nil, err
	}
	if _, _, err := ParseBlock(config); err != nil {
		return nil, refuse("%s.", strings.TrimSuffix(err.Error(), "."))
	}
	return config, nil
}

// backup copies config to BackupDir, keeping the newest few, and returns
// the backup's name.
func (h *Host) backup(config []byte) (string, error) {
	dir := h.path(BackupDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if fi, err := os.Lstat(dir); err != nil {
		return "", err
	} else if !fi.IsDir() {
		return "", fmt.Errorf("%s isn't a folder", dir)
	}
	name := "config-" + h.now().UTC().Format("20060102T150405Z") + ".txt"
	if err := writeAtomic(filepath.Join(dir, name), config, 0o644); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
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
			return "", err
		}
	}
	return name, nil
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
