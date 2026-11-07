package pifirmware

// What pi-firmware-boot.service runs as root: Boot at every boot, and
// CleanStop at every clean shutdown.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"
)

// Boot runs once at every boot. It:
//  1. marks the boot as running, noting whether the last one ended
//     uncleanly (a crash, a watchdog reset or a power cut);
//  2. puts back the CPU governor admin set;
//  3. watches kept settings on probation, and puts the old config.txt
//     back if they reset the Pi too often;
//  4. settles a trial: on a trial boot it keeps the Minecraft server
//     stopped, checks the Pi, waits for Keep, and reboots back to
//     config.txt if a check fails or Keep doesn't come in time.
//
// On a trial boot it runs until the trial is kept or the Pi reboots.
func (h *Host) Boot(ctx context.Context) error {
	unclean, err := h.markBoot()
	errs := []error{err}
	if b, err := readFile(h.path(governorPath)); err == nil {
		name := strings.TrimSpace(string(b))
		if _, offered := h.governors(); slices.Contains(offered, name) {
			errs = append(errs, h.applyGovernor(name))
		}
	}
	rolledBack, err := h.probation(ctx, unclean)
	errs = append(errs, err)
	if !rolledBack {
		errs = append(errs, h.settleTrial(ctx))
	}
	return errors.Join(errs...)
}

// CleanStop runs at a clean shutdown: the boot ended cleanly.
func (h *Host) CleanStop() error {
	return removeFile(h.path(markerPath))
}

func (h *Host) markBoot() (unclean bool, err error) {
	_, err = os.Lstat(h.path(markerPath))
	unclean = err == nil
	if err := os.MkdirAll(h.path(StateDir), 0o755); err != nil {
		return unclean, err
	}
	return unclean, writeAtomic(h.path(markerPath), []byte(h.now().UTC().Format(time.RFC3339)+"\n"), 0o644)
}

// probation counts an unclean boot against settings kept less than
// Probation ago, and at ProbationResets puts back the config.txt from
// before they were kept, and reboots onto it.
func (h *Host) probation(ctx context.Context, unclean bool) (bool, error) {
	if !unclean {
		return false, nil
	}
	unlock, err := h.lock()
	if err != nil {
		return false, err
	}
	defer unlock()
	t, err := h.readState()
	if err != nil || t == nil || t.Phase != PhaseKept || !h.now().Before(t.At.Add(Probation)) {
		return false, err
	}
	t.Resets++
	if t.Resets < ProbationResets {
		t.Message = fmt.Sprintf("The Pi reset uncleanly %d time since these settings were kept. At %d, the old config.txt comes back by itself.",
			t.Resets, ProbationResets)
		return false, h.writeState(t)
	}
	name := filepath.Base(t.Backup)
	if !strings.HasPrefix(name, "config-") || !strings.HasSuffix(name, ".txt") {
		t.Message = "The kept settings reset the Pi too often, but there's no backup of config.txt to put back. Fix config.txt by hand."
		return false, h.writeState(t)
	}
	old, err := readFile(filepath.Join(h.path(BackupDir), name))
	if err != nil {
		t.Message = fmt.Sprintf("The kept settings reset the Pi too often, but the backup %s can't be read (%v). Fix config.txt by hand.", name, err)
		return false, errors.Join(err, h.writeState(t))
	}
	if err := writeAtomic(h.path(ConfigPath), old, 0o644); err != nil {
		return false, err
	}
	t.Phase, t.At = PhaseRolledBack, h.now()
	t.Message = fmt.Sprintf("The kept settings reset the Pi %d times, so the config.txt from before them (%s) was put back, and the Pi rebooted onto it.",
		t.Resets, name)
	if err := h.writeState(t); err != nil {
		return false, err
	}
	return true, h.Run(ctx, "systemctl", "reboot")
}

func (h *Host) keepWindow() time.Duration {
	if h.KeepWindow > 0 {
		return h.KeepWindow
	}
	return KeepWindow
}

func (h *Host) pollEvery() time.Duration {
	if h.PollEvery > 0 {
		return h.PollEvery
	}
	return time.Second
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
	if !h.TrialBoot() {
		defer unlock()
		if t == nil {
			return nil
		}
		switch t.Phase {
		case PhaseRebooting, PhaseChecking:
			t.Phase = PhaseFailed
			t.Message = "The Pi didn't finish the trial boot (it hung, crashed or reset), so it's back on the saved settings."
		case PhasePassed:
			if s, found, err := h.savedSettings(); err == nil && found && t.Backup != "" && reflect.DeepEqual(s, t.Settings) {
				// Keep wrote config.txt, then the Pi lost power before
				// the state said so.
				h.markKept(t)
				return errors.Join(removeFile(h.path(TrybootPath)), h.writeState(t))
			}
			t.Phase = PhaseExpired
			t.Message = "The Pi rebooted before the trial settings were kept, so it's back on the saved settings."
		case PhaseDiscarded:
			t.Phase = PhaseReverted
			t.Message = "Went back: the Pi is on the saved settings again."
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

	// Paper stays off untested settings until they're kept: anything that
	// starts minecraft.service is stopped again.
	holdCtx, stopHold := context.WithCancel(ctx)
	defer stopHold()
	go h.holdServer(holdCtx)

	checkErr := h.check(ctx, t.Settings)
	if checkErr == nil && h.serverUp() {
		// The hold has had the whole check to stop it.
		checkErr = errors.New("the Minecraft server kept running during the trial, and couldn't be stopped")
	}

	unlock, err = h.lock()
	if err != nil {
		return err
	}
	if t, err = h.readState(); err != nil || t == nil {
		unlock()
		return err
	}
	if checkErr != nil && (t.Phase == PhaseChecking || t.Phase == PhaseDiscarded) {
		// Failed, even if Go back came first: don't leave failing settings
		// running until someone reboots.
		t.Phase, t.At = PhaseFailed, h.now()
		t.Message = fmt.Sprintf("The new settings failed a check (%v), so the Pi rebooted back to the saved settings.", checkErr)
		err := errors.Join(removeFile(h.path(TrybootPath)), h.writeState(t))
		unlock()
		if err != nil {
			return err
		}
		return h.reboot(ctx)
	}
	if t.Phase == PhaseChecking {
		t.Phase, t.At = PhasePassed, h.now()
		t.Message = fmt.Sprintf("The new settings passed their checks. Keep them within %d minutes, or the Pi goes back by itself.",
			int(h.keepWindow().Minutes()))
		err = h.writeState(t)
	}
	unlock()
	if err != nil {
		return err
	}
	return h.awaitKeep(ctx)
}

// awaitKeep holds the server off while a trial that passed waits for
// Keep, and reboots back to config.txt if Keep doesn't come within the
// keep window. It returns once the trial is kept.
func (h *Host) awaitKeep(ctx context.Context) error {
	deadline := time.Now().Add(h.keepWindow())
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(h.pollEvery()):
		}
		unlock, err := h.lock()
		if err != nil {
			return err
		}
		t, err := h.readState()
		if err != nil || t == nil || (t.Phase != PhasePassed && t.Phase != PhaseDiscarded) {
			unlock()
			return err // kept (or settled some other way): the server may start
		}
		if t.Phase == PhasePassed && !time.Now().Before(deadline) {
			t.Phase, t.At = PhaseExpired, h.now()
			t.Message = fmt.Sprintf("The trial settings weren't kept within %d minutes, so the Pi rebooted back to the saved settings.",
				int(h.keepWindow().Minutes()))
			err := errors.Join(removeFile(h.path(TrybootPath)), h.writeState(t))
			unlock()
			if err != nil {
				return err
			}
			return h.reboot(ctx)
		}
		unlock()
	}
}

// reboot stops the server first, so Paper saves, then reboots normally.
// A server that won't stop doesn't hold up going back.
func (h *Host) reboot(ctx context.Context) error {
	var stopErr error
	if h.serverUp() {
		stopErr = h.stopServer(ctx)
	}
	return errors.Join(stopErr, h.Run(ctx, "systemctl", "reboot"))
}

// holdServer stops minecraft.service whenever it's running, until ctx
// ends.
func (h *Host) holdServer(ctx context.Context) {
	for {
		if h.serverUp() {
			h.stopServer(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(h.pollEvery()):
		}
	}
}

func (h *Host) savedSettings() (Settings, bool, error) {
	b, err := readFile(h.path(ConfigPath))
	if err != nil {
		return Settings{}, false, err
	}
	return ParseBlock(b)
}

// check is a trial boot's health check. It runs the CPU load under the
// performance governor, and fails on under-voltage before or during it, a
// capped clock or throttling during it, a wrong answer from the load, or
// a clock that never reaches the arm_freq being tried. admin's health
// check isn't part of it: anything running as admin's user could answer
// that.
func (h *Host) check(ctx context.Context, s Settings) error {
	if h.Throttled == nil || h.Load == nil {
		return errors.New("no throttle reading or CPU check")
	}
	before, err := h.Throttled(ctx)
	if err != nil {
		return fmt.Errorf("reading get_throttled: %w", err)
	}
	if before&(underVoltage|underVoltage<<sinceShift) != 0 {
		return errors.New("under-voltage before the load")
	}

	if prev, offered := h.governors(); slices.Contains(offered, "performance") {
		if err := h.applyGovernor("performance"); err != nil {
			return err
		}
		defer h.applyGovernor(prev)
	}

	loadCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.Load(loadCtx) }()
	maxKHz := 0
	sample := func() error {
		v, err := h.Throttled(ctx)
		if err != nil {
			return fmt.Errorf("reading get_throttled: %w", err)
		}
		if v&badBits != 0 {
			return fmt.Errorf("%s during the load", describeThrottle(v))
		}
		if k, err := h.curKHz(); err == nil {
			maxKHz = max(maxKHz, k)
		}
		return nil
	}
	tick := time.NewTicker(h.pollEvery())
	defer tick.Stop()
	for running := true; running; {
		select {
		case err := <-done:
			if err != nil {
				return fmt.Errorf("CPU check: %w", err)
			}
			running = false
		case <-tick.C:
			if err := sample(); err != nil {
				cancel()
				<-done
				return err
			}
		}
	}
	if err := sample(); err != nil {
		return err
	}

	wantMHz := Limits.ArmFreq.Default
	if s.ArmFreq != nil {
		wantMHz = *s.ArmFreq
	}
	switch {
	case maxKHz == 0:
		return errors.New("couldn't read the CPU clock during the load")
	case maxKHz < wantMHz*1000*97/100:
		return fmt.Errorf("the CPU only reached %d of %d MHz under load", maxKHz/1000, wantMHz)
	}
	return nil
}
