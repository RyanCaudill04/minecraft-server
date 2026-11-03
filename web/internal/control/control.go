// Package control starts, stops and restarts the Minecraft server's user
// unit through systemctl --user, and reports its state.
//
// minecraft.service runs LazyMC, which starts Paper on demand, so stopping
// the unit stops both; a stop can take minutes while Paper saves. Actions
// run with --no-block, so they return as soon as systemd has queued the
// job, and the state shows the progress (deactivating, then inactive).
//
// systemctl always gets fixed arguments: the action is one of three
// constants and the unit is checked against an allowlist, so nothing from
// a request ever reaches the command line.
package control

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Unit is the only unit control may act on.
const Unit = "minecraft.service"

// Action is something to do to the unit.
type Action string

// The actions.
const (
	Start   Action = "start"
	Stop    Action = "stop"
	Restart Action = "restart"
)

// ParseAction returns the action named s, or false.
func ParseAction(s string) (Action, bool) {
	switch a := Action(s); a {
	case Start, Stop, Restart:
		return a, true
	}
	return "", false
}

// ErrBusy means another action is running, or the unit is still starting
// or stopping from the last one.
var ErrBusy = errors.New("control: another action is in progress")

// State is the unit's state from systemd: Active is active, activating,
// deactivating, inactive, failed, reloading or maintenance; Sub is the
// finer state, such as running, stop-sigterm or dead.
type State struct {
	Active string `json:"active"`
	Sub    string `json:"sub"`
}

// Busy reports whether the unit is between states.
func (s State) Busy() bool {
	switch s.Active {
	case "activating", "deactivating", "reloading", "maintenance":
		return true
	}
	return false
}

// Controller acts on the Minecraft server. Systemd is the real one; admin's
// tests use a fake.
type Controller interface {
	Do(ctx context.Context, a Action) error
	State(ctx context.Context) (State, error)
}

// Runner runs a command and returns its standard output. exec.CommandContext
// in Systemd, a fake in tests.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Systemd controls one user unit through systemctl --user.
type Systemd struct {
	unit    string
	run     Runner
	timeout time.Duration

	mu sync.Mutex // held while an action runs
}

// NewSystemd returns a controller for unit, which must be Unit. run may be
// nil to run the real systemctl.
func NewSystemd(unit string, run Runner) (*Systemd, error) {
	if unit != Unit {
		return nil, fmt.Errorf("control: unit %q isn't allowed, only %s", unit, Unit)
	}
	if run == nil {
		run = execRun
	}
	return &Systemd{unit: unit, run: run, timeout: 15 * time.Second}, nil
}

// Do runs the action without waiting for the unit to finish starting or
// stopping. It returns ErrBusy if another action is running or the unit is
// between states.
func (s *Systemd) Do(ctx context.Context, a Action) error {
	if _, ok := ParseAction(string(a)); !ok {
		return fmt.Errorf("control: unknown action %q", a)
	}
	if !s.mu.TryLock() {
		return ErrBusy
	}
	defer s.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	st, err := s.state(ctx)
	if err != nil {
		return err
	}
	if st.Busy() {
		return ErrBusy
	}
	if _, err := s.run(ctx, "systemctl", "--user", "--no-block", string(a), s.unit); err != nil {
		return fmt.Errorf("control: systemctl %s %s: %w", a, s.unit, err)
	}
	return nil
}

// State returns the unit's state.
func (s *Systemd) State(ctx context.Context) (State, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	return s.state(ctx)
}

func (s *Systemd) state(ctx context.Context) (State, error) {
	out, err := s.run(ctx, "systemctl", "--user", "show", "--property=ActiveState,SubState", s.unit)
	if err != nil {
		return State{}, fmt.Errorf("control: systemctl show %s: %w", s.unit, err)
	}
	var st State
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), "=")
		switch k {
		case "ActiveState":
			st.Active = v
		case "SubState":
			st.Sub = v
		}
	}
	if st.Active == "" {
		return State{}, fmt.Errorf("control: systemctl show %s: no ActiveState in %q", s.unit, out)
	}
	return st, nil
}

// execRun runs a command with no shell, and puts its stderr in the error.
func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return out, fmt.Errorf("%w: %s", err, msg)
		}
		return out, err
	}
	return out, nil
}
