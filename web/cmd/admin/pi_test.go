package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/control"
	"github.com/ryancaudill04/minecraft-server/web/internal/mcstatus"
	"github.com/ryancaudill04/minecraft-server/web/internal/pifirmware"
)

const owner = "owner@example.com"

// fakeHelper records the pi-firmware commands admin runs.
type fakeHelper struct {
	mu    sync.Mutex
	calls []string // "cmd input"
	err   error
}

func (f *fakeHelper) run(_ context.Context, cmd string, input []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.TrimSpace(cmd+" "+string(input)))
	return f.err
}

func (f *fakeHelper) ran() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

type piServer struct {
	*controlServer
	helper *fakeHelper
	root   string // the synthetic Pi
}

// newPiServer serves admin as user, with owner allowed to change the Pi,
// a fake helper, and a synthetic Pi 5 as hostRoot.
func newPiServer(t *testing.T, user string) *piServer {
	t.Helper()
	root := t.TempDir()
	for rel, content := range map[string]string{
		"proc/device-tree/model":             "Raspberry Pi 5 Model B Rev 1.0\x00",
		pifirmware.ConfigPath:                "arm_64bit=1\n",
		"sys/class/watchdog/watchdog0/state": "active\n",
	} {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
	}
	out := &syncBuffer{}
	log.SetOutput(out)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	helper := &fakeHelper{}
	ctl := &fakeControl{state: control.State{Active: "inactive", Sub: "dead"}}
	wake := &fakeServer{state: mcstatus.StateSleeping}
	a := &app{
		dir: t.TempDir(), sharedDir: t.TempDir(), appsDir: t.TempDir(), serverDir: t.TempDir(),
		checker:  &mcstatus.Checker{PaperAddr: "paper", LazymcAddr: "lazymc", Timeout: time.Second, Ping: (&fakeServer{}).ping},
		control:  ctl,
		hostRoot: root,
		wake:     wake.wake,
		pi:       helper.run,
		piOwners: []string{"someone@example.com", "Owner@Example.com"},
		user:     func(*http.Request) string { return user },
	}
	ts := httptest.NewServer(newHandler(a))
	t.Cleanup(ts.Close)
	return &piServer{controlServer: &controlServer{Server: ts, ctl: ctl, out: out}, helper: helper, root: root}
}

func (s *piServer) setUnit(active string) {
	s.ctl.mu.Lock()
	defer s.ctl.mu.Unlock()
	s.ctl.state = control.State{Active: active}
}

func TestPiStatus(t *testing.T) {
	for user, can := range map[string]bool{owner: true, "guest@example.com": false} {
		s := newPiServer(t, user)
		resp, err := http.Get(s.URL + "/api/pi")
		if err != nil {
			t.Fatal(err)
		}
		var st PiStatus
		json.NewDecoder(resp.Body).Decode(&st)
		resp.Body.Close()
		if !st.Supported || !st.Watchdog || st.CanChange != can || st.Limits.ArmFreq.Max != 3000 {
			t.Errorf("%s: GET /api/pi = %+v", user, st)
		}
	}
	s := newPiServer(t, "")
	resp, _ := http.Get(s.URL + "/api/pi")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("GET /api/pi without a user = %d", resp.StatusCode)
	}
}

func TestPiStage(t *testing.T) {
	s := newPiServer(t, owner)
	code, got := s.post(t, "/api/pi/settings", `{"arm_freq": 2600, "temp_limit": 80}`, nil)
	if code != http.StatusOK {
		t.Fatalf("stage = %d %v", code, got)
	}
	// The helper gets admin's own encoding, not the request's bytes.
	want := []string{`stage {"arm_freq":2600,"temp_limit":80}`}
	if got := s.helper.ran(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("ran %q, want %q", got, want)
	}
	if !strings.Contains(s.out.String(), "admin: audit: "+owner+": pi stage") {
		t.Errorf("no audit line in %q", s.out.String())
	}
}

func TestPiStageRefusesBadInput(t *testing.T) {
	s := newPiServer(t, owner)
	for _, body := range []string{
		`{"arm_freq": 4000}`,
		`{"arm_freq": "2600\nforce_turbo=1"}`,
		`{"force_turbo": 1}`,
		`{"arm_freq": 2600.5}`,
		`{"arm_freq": 2600}{"arm_freq": 3000}`,
		`{"fan": [{"temp": 50000, "hyst": 5000, "speed": 0}]}`,
	} {
		if code, _ := s.post(t, "/api/pi/settings", body, nil); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", body, code)
		}
	}
	if code, _ := s.post(t, "/api/pi/settings", `{}`, map[string]string{"Content-Type": "text/plain"}); code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain: %d", code)
	}
	if got := s.helper.ran(); len(got) != 0 {
		t.Errorf("ran %q for bad input", got)
	}
}

// Only the owner, same-origin, with the CSRF header.
func TestPiWritesNeedOwner(t *testing.T) {
	paths := []string{"/api/pi/settings", "/api/pi/settings/keep", "/api/pi/settings/discard", "/api/pi/governor", "/api/pi/reboot"}
	guest := newPiServer(t, "guest@example.com")
	for _, p := range paths {
		if code, got := guest.post(t, p, `{}`, nil); code != http.StatusForbidden || got["error"] != lockedPiOwner {
			t.Errorf("guest %s = %d %v", p, code, got)
		}
	}
	s := newPiServer(t, owner)
	for _, p := range paths {
		if code, _ := s.post(t, p, `{}`, map[string]string{CSRFHeader: ""}); code != http.StatusForbidden {
			t.Errorf("%s without the header = %d", p, code)
		}
		if code, _ := s.post(t, p, `{}`, map[string]string{"Sec-Fetch-Site": "cross-site"}); code != http.StatusForbidden {
			t.Errorf("%s cross-site = %d", p, code)
		}
	}
	if len(guest.helper.ran())+len(s.helper.ran()) != 0 {
		t.Error("the helper ran for a refused request")
	}
}

func TestPiLockedWithoutOwners(t *testing.T) {
	a := &app{pi: (&fakeHelper{}).run}
	if got := a.piLocked(owner); got != lockedPiNoOwner {
		t.Errorf("piLocked = %q", got)
	}
	a.piOwners = []string{owner}
	a.piInstalled = func() bool { return false }
	if got := a.piLocked(owner); got != lockedPiSetUp {
		t.Errorf("piLocked without the helper = %q", got)
	}
}

func TestPiReboot(t *testing.T) {
	s := newPiServer(t, owner)
	// The word must be typed.
	for _, body := range []string{`{"trial": true}`, `{"confirm": "yes", "trial": true}`, `{"confirm": "Reboot"}`} {
		if code, _ := s.post(t, "/api/pi/reboot", body, nil); code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", body, code)
		}
	}
	// The server must be stopped.
	s.setUnit("active")
	if code, _ := s.post(t, "/api/pi/reboot", `{"confirm": "reboot", "trial": true}`, nil); code != http.StatusConflict {
		t.Errorf("reboot with the server running = %d, want 409", code)
	}
	if got := s.helper.ran(); len(got) != 0 {
		t.Fatalf("ran %q", got)
	}
	s.setUnit("inactive")
	if code, got := s.post(t, "/api/pi/reboot", `{"confirm": "reboot", "trial": true}`, nil); code != http.StatusAccepted {
		t.Fatalf("trial reboot = %d %v", code, got)
	}
	if code, _ := s.post(t, "/api/pi/reboot", `{"confirm": "reboot"}`, nil); code != http.StatusAccepted {
		t.Fatalf("reboot = %d", code)
	}
	if got := strings.Join(s.helper.ran(), "|"); got != "reboot-trial|reboot" {
		t.Errorf("ran %q", got)
	}
}

func TestPiHelperErrors(t *testing.T) {
	s := newPiServer(t, owner)
	s.helper.err = &pifirmware.RefusedError{Msg: "Keep them or go back first."}
	if code, got := s.post(t, "/api/pi/settings/keep", ``, nil); code != http.StatusConflict || got["error"] != "Keep them or go back first." {
		t.Errorf("refused = %d %v", code, got)
	}
	s.helper.err = errors.New("sudo: a password is required")
	code, got := s.post(t, "/api/pi/settings/discard", ``, nil)
	if code != http.StatusBadGateway || strings.Contains(got["error"], "password") {
		t.Errorf("failed = %d %v", code, got)
	}
}

func TestPiGovernor(t *testing.T) {
	s := newPiServer(t, owner)
	if code, _ := s.post(t, "/api/pi/governor", `{"governor": "userspace"}`, nil); code != http.StatusBadRequest {
		t.Errorf("userspace = %d", code)
	}
	if code, _ := s.post(t, "/api/pi/governor", `{"governor": "performance"}`, nil); code != http.StatusOK {
		t.Errorf("performance = %d", code)
	}
	if got := s.helper.ran(); len(got) != 1 || got[0] != `governor {"governor":"performance"}` {
		t.Errorf("ran %q", got)
	}
}

func TestCleanMessage(t *testing.T) {
	for in, want := range map[string]string{
		"Keep them first.\n":           "Keep them first.",
		"one\ntwo":                     "one",
		"bell\a and \x1b[31mred":       "bell and [31mred",
		"":                             "pi-firmware refused.",
		strings.Repeat("x", 400) + "y": strings.Repeat("x", 300),
	} {
		if got := cleanMessage(in); got != want {
			t.Errorf("cleanMessage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOwners(t *testing.T) {
	if got := owners(" a@example.com, ,b@example.com "); strings.Join(got, "|") != "a@example.com|b@example.com" {
		t.Errorf("owners = %q", got)
	}
	if got := owners(""); got != nil {
		t.Errorf("owners(\"\") = %q", got)
	}
}

// While the Pi runs settings on trial, the server can be stopped but not
// started, restarted or woken.
func TestServerStaysOffDuringTrial(t *testing.T) {
	s := newPiServer(t, owner)
	flag := filepath.Join(s.root, "proc/device-tree/chosen/bootloader/tryboot")
	os.MkdirAll(filepath.Dir(flag), 0o755)
	os.WriteFile(flag, []byte{0, 0, 0, 1}, 0o644)
	for _, path := range []string{"/api/server/start", "/api/server/restart", "/api/server/wake"} {
		if code, got := s.post(t, path, ``, nil); code != http.StatusConflict || got["error"] != lockedTrial {
			t.Errorf("%s = %d %v", path, code, got)
		}
	}
	s.setUnit("active")
	if code, _ := s.post(t, "/api/server/stop", ``, nil); code != http.StatusAccepted {
		t.Errorf("stop = %d", code)
	}
	if got := s.ctl.done(); len(got) != 1 || got[0] != control.Stop {
		t.Errorf("actions %q", got)
	}
}
