package pifirmware

// What admin asks for, through the sudo rule. Each one takes the lock,
// reads the state fresh and refuses what doesn't fit the trial's phase.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"slices"
)

// gate is what the last trial allows next.
type gate int

const (
	gateFree          gate = iota
	gateRebootPending      // RebootTrial ran, and the Pi hasn't rebooted yet
	gateOnTrial            // running on tryboot.txt, not yet kept or dropped
	gateGoingBack          // Go back during a trial; a reboot finishes it
)

func (h *Host) gate(t *Trial) gate {
	if t == nil {
		return gateFree
	}
	trial := h.TrialBoot()
	switch {
	case !trial && t.Phase == PhaseRebooting:
		return gateRebootPending
	case trial && (t.Phase == PhaseRebooting || t.Phase == PhaseChecking || t.Phase == PhasePassed):
		return gateOnTrial
	case trial && t.Phase == PhaseDiscarded:
		return gateGoingBack
	}
	return gateFree
}

const (
	msgRebootPending = "A reboot onto the trial settings is under way. If the Pi didn't reboot, try the reboot again."
	msgOnTrial       = "The Pi is trying new settings right now. Keep them or go back first."
	msgGoingBack     = "Going back to the saved settings: reboot to finish first."
	serverUpMsg      = "Stop the server first, so Paper saves the world before the Pi reboots."
)

// ready refuses unless the Pi is set up to try settings safely: the boot
// unit that checks a trial and watches kept settings, and the hardware
// watchdog that every admin block arms.
func (h *Host) ready() error {
	if !h.bootCheck() {
		return refuse("pi-firmware-boot.service isn't enabled, so nothing would check the settings. Set it up first (deploy/pi-firmware/README.md).")
	}
	if !h.watchdog() {
		return refuse("The hardware watchdog isn't running, so settings that hang the Pi couldn't reset it. Set it up first (deploy/pi-firmware/README.md).")
	}
	return nil
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
	if err := h.ready(); err != nil {
		return err
	}
	t, err := h.readState()
	if err != nil {
		return err
	}
	switch h.gate(t) {
	case gateRebootPending:
		return refuse(msgRebootPending)
	case gateOnTrial:
		return refuse(msgOnTrial)
	case gateGoingBack:
		return refuse(msgGoingBack)
	}
	config, err := h.readConfig()
	if err != nil {
		return err
	}
	out, err := Splice(config, Block(s))
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
	g := h.gate(t)
	switch g {
	case gateRebootPending:
		return refuse(msgRebootPending)
	case gateGoingBack:
		return refuse(msgGoingBack)
	}
	if err := removeFile(h.path(TrybootPath)); err != nil {
		return err
	}
	if g == gateOnTrial {
		t.Phase, t.At = PhaseDiscarded, h.now()
		t.Message = "Going back: reboot to return to the saved settings."
		return h.writeState(t)
	}
	return nil
}

// Keep saves the settings a trial boot passed with into config.txt,
// after backing config.txt up, and removes tryboot.txt. The kept settings
// then go on probation (see Boot).
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
	if !h.TrialBoot() || t == nil || t.Phase != PhasePassed {
		return refuse("Only settings that passed a trial boot can be kept.")
	}
	tb, err := readFile(h.path(TrybootPath))
	if err != nil {
		return refuse("tryboot.txt is gone, so there's nothing to keep.")
	}
	s, found, err := ParseBlock(tb)
	if err != nil || !found {
		return refuse("tryboot.txt isn't the one admin wrote, so it can't be kept.")
	}
	if !reflect.DeepEqual(s, t.Settings) {
		return refuse("tryboot.txt changed since the trial boot, so it can't be kept.")
	}
	config, err := h.readConfig()
	if err != nil {
		return err
	}
	out, err := Splice(config, Block(s))
	if err != nil {
		return refuse("%s.", err)
	}
	name, err := h.backup(config)
	if err != nil {
		return fmt.Errorf("backing up config.txt: %w", err)
	}
	// The backup's name goes in the state before config.txt changes, so a
	// power cut in between still leaves probation something to restore,
	// and Boot can tell the settings were kept (settleTrial).
	t.Backup = name
	if err := h.writeState(t); err != nil {
		return err
	}
	if err := writeAtomic(h.path(ConfigPath), out, 0o644); err != nil {
		return err
	}
	if err := removeFile(h.path(TrybootPath)); err != nil {
		return err
	}
	h.markKept(t)
	return h.writeState(t)
}

// markKept puts t on probation.
func (h *Host) markKept(t *Trial) {
	t.Phase, t.At, t.Resets = PhaseKept, h.now(), 0
	t.Message = fmt.Sprintf("Saved to config.txt. For %d days, if the Pi resets uncleanly %d times, the old config.txt comes back by itself.",
		int(Probation.Hours()/24), ProbationResets)
}

// RebootTrial reboots the Pi once onto tryboot.txt. It refuses unless
// tryboot.txt is admin's, the Minecraft server is stopped, and the boot
// check and hardware watchdog are set up, so a trial that hangs resets
// itself back onto config.txt.
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
	switch h.gate(t) {
	case gateOnTrial:
		return refuse(msgOnTrial)
	case gateGoingBack:
		return refuse(msgGoingBack)
	}
	tb, err := readFile(h.path(TrybootPath))
	if errors.Is(err, fs.ErrNotExist) {
		return refuse("No settings are waiting for a trial. Save some first.")
	} else if err != nil {
		return err
	}
	s, found, err := ParseBlock(tb)
	if err != nil || !found {
		return refuse("tryboot.txt isn't the one admin wrote, so admin won't boot it.")
	}
	if h.serverUp() {
		return refuse(serverUpMsg)
	}
	if err := h.ready(); err != nil {
		return err
	}
	if err := h.writeState(&Trial{Phase: PhaseRebooting, Settings: s, At: h.now(),
		Message: "Rebooting to try the new settings."}); err != nil {
		return err
	}
	// systemctl passes "0 tryboot" to the reboot system call: partition 0
	// (the default), and the one-shot tryboot flag. If it fails, the state
	// stays "rebooting": systemctl can fail after queuing the reboot, and
	// the trial boot must still be checked. Boot marks it failed on the
	// next normal boot, and it can be retried.
	return h.Run(ctx, "systemctl", "reboot", "0 tryboot")
}

// Reboot reboots the Pi normally, onto config.txt.
func (h *Host) Reboot(ctx context.Context) error {
	unlock, err := h.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if h.serverUp() {
		return refuse(serverUpMsg)
	}
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
