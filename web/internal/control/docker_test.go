package control

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDocker is a stand-in for the Engine API: it serves one container's
// state and records the actions posted to it. block, if set, holds each
// action until it's closed.
type fakeDocker struct {
	mu       sync.Mutex
	status   string
	exitCode int
	missing  bool
	posts    []string
	block    chan struct{}
	fail     int // status code for actions, if not 0
}

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	missing, block, fail := f.missing, f.block, f.fail
	f.mu.Unlock()
	if missing {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"No such container: mc-server"}`))
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/containers/mc-server/json":
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Write([]byte(`{"Id":"abc","State":{"Status":"` + f.status + `","ExitCode":` + strconv.Itoa(f.exitCode) + `}}`))
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/containers/mc-server/"):
		f.mu.Lock()
		f.posts = append(f.posts, r.URL.RequestURI())
		f.mu.Unlock()
		if block != nil {
			<-block
		}
		if fail != 0 {
			w.WriteHeader(fail)
			w.Write([]byte(`{"message":"it broke"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeDocker) set(status string) {
	f.mu.Lock()
	f.status = status
	f.mu.Unlock()
}

func (f *fakeDocker) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.posts...)
}

func newFakeDocker(t *testing.T, f *fakeDocker) *Docker {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	d, err := newDocker("mc-server", srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	d.log = func(string, ...any) {}
	return d
}

func TestNewDockerRejectsBadNames(t *testing.T) {
	for _, name := range []string{"", "../mc-server", "mc-server/stop", "-x", "mc server"} {
		if _, err := NewDocker(name, DockerSocket); err == nil {
			t.Errorf("NewDocker(%q) accepted it", name)
		}
	}
	if _, err := NewDocker(Container, DockerSocket); err != nil {
		t.Errorf("NewDocker(%q): %v", Container, err)
	}
}

func TestDockerState(t *testing.T) {
	for _, c := range []struct {
		status string
		code   int
		want   State
	}{
		{"running", 0, State{"active", "running"}},
		{"paused", 0, State{"active", "paused"}},
		{"restarting", 0, State{"activating", "auto-restart"}},
		{"removing", 0, State{"deactivating", "removing"}},
		{"created", 0, State{"inactive", "dead"}},
		{"exited", 0, State{"inactive", "dead"}},
		{"exited", 143, State{"inactive", "dead"}},
		{"exited", 1, State{"failed", "exit-code"}},
		{"dead", 0, State{"failed", "failed"}},
	} {
		d := newFakeDocker(t, &fakeDocker{status: c.status, exitCode: c.code})
		got, err := d.State(context.Background())
		if err != nil {
			t.Fatalf("%s/%d: %v", c.status, c.code, err)
		}
		if got != c.want {
			t.Errorf("%s exit %d = %+v, want %+v", c.status, c.code, got, c.want)
		}
	}
}

func TestDockerStateMissingContainer(t *testing.T) {
	d := newFakeDocker(t, &fakeDocker{missing: true})
	_, err := d.State(context.Background())
	if err == nil || !strings.Contains(err.Error(), "No such container") {
		t.Errorf("State = %v, want the API's error", err)
	}
}

func TestDockerActions(t *testing.T) {
	for _, c := range []struct {
		status string
		action Action
		want   string
	}{
		{"exited", Start, "/containers/mc-server/start"},
		{"running", Stop, "/containers/mc-server/stop?t=240"},
		{"running", Restart, "/containers/mc-server/restart?t=240"},
	} {
		f := &fakeDocker{status: c.status}
		d := newFakeDocker(t, f)
		if err := d.Do(context.Background(), c.action); err != nil {
			t.Fatalf("%s: %v", c.action, err)
		}
		if got := f.actions(); len(got) != 1 || got[0] != c.want {
			t.Errorf("%s posted %q, want %q", c.action, got, c.want)
		}
	}
}

func TestDockerRejectsUnknownAction(t *testing.T) {
	f := &fakeDocker{status: "running"}
	d := newFakeDocker(t, f)
	for _, a := range []Action{"kill", "", "stop/../../x"} {
		if err := d.Do(context.Background(), a); err == nil {
			t.Errorf("Do(%q) succeeded", a)
		}
	}
	if got := f.actions(); len(got) != 0 {
		t.Errorf("posted %q", got)
	}
}

// A slow stop returns at once, shows as deactivating until Docker answers,
// and refuses a second action meanwhile.
func TestDockerSlowStopRunsInBackground(t *testing.T) {
	f := &fakeDocker{status: "running", block: make(chan struct{})}
	d := newFakeDocker(t, f)
	d.wait = 10 * time.Millisecond

	if err := d.Do(context.Background(), Stop); err != nil {
		t.Fatal(err)
	}
	st, err := d.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st != (State{"deactivating", "stop-sigterm"}) || !st.Busy() {
		t.Errorf("during the stop, State = %+v, want deactivating", st)
	}
	if err := d.Do(context.Background(), Start); !errors.Is(err, ErrBusy) {
		t.Errorf("a second action = %v, want ErrBusy", err)
	}

	f.set("exited")
	close(f.block)
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, err = d.State(context.Background())
		if err == nil && st.Active == "inactive" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the stop, State = %+v, %v; want inactive", st, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := f.actions(); len(got) != 1 {
		t.Errorf("posted %q, want only the stop", got)
	}
}

// A container that's already between states refuses an action.
func TestDockerBusyContainer(t *testing.T) {
	f := &fakeDocker{status: "restarting"}
	d := newFakeDocker(t, f)
	if err := d.Do(context.Background(), Stop); !errors.Is(err, ErrBusy) {
		t.Errorf("Do = %v, want ErrBusy", err)
	}
	if got := f.actions(); len(got) != 0 {
		t.Errorf("posted %q", got)
	}
	// And the refusal doesn't leave anything pending.
	f.set("running")
	if err := d.Do(context.Background(), Stop); err != nil {
		t.Errorf("after it settled, Do = %v", err)
	}
}

// A quick failure, such as Docker refusing the action, is returned.
func TestDockerActionError(t *testing.T) {
	f := &fakeDocker{status: "exited", fail: http.StatusInternalServerError}
	d := newFakeDocker(t, f)
	err := d.Do(context.Background(), Start)
	if err == nil || !strings.Contains(err.Error(), "it broke") {
		t.Errorf("Do = %v, want the API's error", err)
	}
	if st, _ := d.State(context.Background()); st.Busy() {
		t.Errorf("after a failed start, State = %+v, want it settled", st)
	}
}
