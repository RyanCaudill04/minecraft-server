package main

// The Pi settings: the Raspberry Pi 5's clock, voltage, temperature limit
// and fan curve, tried on one boot before they're kept, and the CPU
// governor. Changing them needs root, so admin runs the pi-firmware
// helper through sudo; see package pifirmware and deploy/pi-firmware.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/ryancaudill04/minecraft-server/web/internal/pifirmware"
	"github.com/ryancaudill04/minecraft-server/web/internal/site"
)

// PiHelper is where the root helper is installed by hand. The sudoers
// rule names this path.
const PiHelper = "/usr/local/sbin/pi-firmware"

const piTimeout = time.Minute

const (
	lockedPiNoOwner = "Locked: set PI_SETTINGS_OWNER in secrets.env to the email that may change the Pi's settings."
	lockedPiOwner   = "Only the Pi's owner can change these settings."
	lockedPiSetUp   = "Not set up on the Pi yet: see deploy/pi-firmware/README.md."
)

// PiStatus is the JSON body of GET /api/pi.
type PiStatus struct {
	pifirmware.Status
	Limits pifirmware.LimitSet `json:"limits"`
	// SetUp is true once the helper is installed.
	SetUp bool `json:"set_up"`
	// CanChange is true if whoever asked may change the settings, and
	// Locked says why not if they can't.
	CanChange bool   `json:"can_change"`
	Locked    string `json:"locked,omitzero"`
}

// piLocked says why user can't change the Pi's settings, or "" if they
// can.
func (a *app) piLocked(user string) string {
	switch {
	case user == "":
		return lockedNoUser
	case len(a.piOwners) == 0:
		return lockedPiNoOwner
	case !slices.ContainsFunc(a.piOwners, func(o string) bool { return strings.EqualFold(o, user) }):
		return lockedPiOwner
	case a.pi == nil || (a.piInstalled != nil && !a.piInstalled()):
		return lockedPiSetUp
	}
	return ""
}

// piStatus handles GET /api/pi.
func (a *app) piStatus(w http.ResponseWriter, r *http.Request, user string) {
	h := &pifirmware.Host{Root: a.hostRoot}
	res := PiStatus{
		Status: h.Status(),
		Limits: pifirmware.Limits,
		SetUp:  a.pi != nil && (a.piInstalled == nil || a.piInstalled()),
		Locked: a.piLocked(user),
	}
	res.CanChange = res.Locked == ""
	site.WriteJSON(w, "admin: pi", res)
}

// piWrites wraps a Pi settings change: everything writes checks, and the
// user must be the owner.
func (a *app) piWrites(h func(w http.ResponseWriter, r *http.Request, user string)) http.Handler {
	return a.writes(func(w http.ResponseWriter, r *http.Request, user string) {
		if msg := a.piLocked(user); msg != "" {
			writeError(w, http.StatusForbidden, msg)
			return
		}
		h(w, r, user)
	})
}

// decodeJSON reads exactly one JSON value with no unknown fields.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "Send JSON.")
		return false
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil || dec.More() {
		writeError(w, http.StatusBadRequest, "Couldn't read that: settings are whole numbers.")
		return false
	}
	return true
}

// runPi runs one helper command and writes the reply.
func (a *app) runPi(w http.ResponseWriter, r *http.Request, user, cmd string, input []byte, code int) {
	log.Printf("admin: audit: %s: pi %s %s", user, cmd, input)
	// Finish even if the browser goes away.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), piTimeout)
	defer cancel()
	err := a.pi(ctx, cmd, input)
	var refused *pifirmware.RefusedError
	switch {
	case errors.As(err, &refused):
		writeError(w, http.StatusConflict, refused.Msg)
	case err != nil:
		log.Printf("admin: pi %s: %v", cmd, err)
		writeError(w, http.StatusBadGateway, "pi-firmware failed: see admin's log.")
	default:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]string{"done": cmd})
	}
}

// stagePi handles POST /api/pi/settings: the settings to try on the next
// trial boot. They're checked here for a quick answer, and again by the
// helper, which doesn't trust admin.
func (a *app) stagePi(w http.ResponseWriter, r *http.Request, user string) {
	var s pifirmware.Settings
	if !decodeJSON(w, r, &s) {
		return
	}
	if err := s.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error()+".")
		return
	}
	b, err := json.Marshal(s)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Couldn't encode the settings.")
		return
	}
	a.runPi(w, r, user, "stage", b, http.StatusOK)
}

// GovernorRequest is the JSON body of POST /api/pi/governor.
type GovernorRequest struct {
	Governor string `json:"governor"`
}

// setGovernor handles POST /api/pi/governor.
func (a *app) setGovernor(w http.ResponseWriter, r *http.Request, user string) {
	var g GovernorRequest
	if !decodeJSON(w, r, &g) {
		return
	}
	if !slices.Contains(pifirmware.Governors, g.Governor) {
		writeError(w, http.StatusBadRequest, "That isn't a governor admin offers.")
		return
	}
	b, _ := json.Marshal(g)
	a.runPi(w, r, user, "governor", b, http.StatusOK)
}

// RebootConfirm is the word the reboot request must carry: the page asks
// for it to be typed, so a stray click can't reboot the Pi.
const RebootConfirm = "reboot"

// RebootRequest is the JSON body of POST /api/pi/reboot. Trial reboots
// once onto the settings waiting in tryboot.txt; otherwise it's a normal
// reboot.
type RebootRequest struct {
	Confirm string `json:"confirm"`
	Trial   bool   `json:"trial"`
}

// rebootPi handles POST /api/pi/reboot. The Minecraft server must be
// stopped first, so Paper has saved and nobody is kicked by surprise.
func (a *app) rebootPi(w http.ResponseWriter, r *http.Request, user string) {
	var req RebootRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Confirm != RebootConfirm {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Type %q to confirm the reboot.", RebootConfirm))
		return
	}
	if a.control == nil {
		writeError(w, http.StatusServiceUnavailable, "Can't check the server is stopped, so the Pi won't reboot.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	st, err := a.control.State(ctx)
	cancel()
	if err != nil {
		log.Printf("admin: pi reboot: %v", err)
		writeError(w, http.StatusBadGateway, "Couldn't check the server is stopped: see admin's log.")
		return
	}
	if st.Active != "inactive" && st.Active != "failed" {
		writeError(w, http.StatusConflict, "Stop the server first, so Paper saves the world before the Pi reboots.")
		return
	}
	cmd := "reboot"
	if req.Trial {
		cmd = "reboot-trial"
	}
	a.runPi(w, r, user, cmd, nil, http.StatusAccepted)
}

// sudoPi runs the helper as root through its sudoers rule, never asking
// for a password. Exit status 2 is a refusal, with a message for the
// page on stderr.
func sudoPi(ctx context.Context, cmd string, input []byte) error {
	c := exec.CommandContext(ctx, "sudo", "-n", PiHelper, cmd)
	c.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	err := c.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 2 {
		return &pifirmware.RefusedError{Msg: cleanMessage(stderr.String())}
	}
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// cleanMessage keeps the first line of a helper's message, without
// control characters, at a sensible length.
func cleanMessage(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if len(s) > 300 {
		s = s[:300]
	}
	if s == "" {
		return "pi-firmware refused."
	}
	return s
}

// helperInstalled reports whether the helper is at PiHelper.
func helperInstalled() bool {
	fi, err := os.Stat(PiHelper)
	return err == nil && fi.Mode().IsRegular()
}

// owners splits PI_SETTINGS_OWNER, a comma-separated list of emails.
func owners(s string) []string {
	var out []string
	for _, e := range strings.Split(s, ",") {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}
