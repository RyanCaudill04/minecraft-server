package control

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// fakeSystemctl records each call and answers show with state.
type fakeSystemctl struct {
	mu    sync.Mutex
	calls [][]string
	state string // systemctl show output
	fail  error  // returned for actions
	block chan struct{}
}

func (f *fakeSystemctl) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{name}, args...))
	state, fail, block := f.state, f.fail, f.block
	f.mu.Unlock()
	if args[1] == "show" {
		return []byte(state), nil
	}
	if block != nil {
		<-block
	}
	return nil, fail
}

func (f *fakeSystemctl) lastCall() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func newFake(t *testing.T, state string) (*Systemd, *fakeSystemctl) {
	t.Helper()
	f := &fakeSystemctl{state: state}
	s, err := NewSystemd(Unit, f.run)
	if err != nil {
		t.Fatal(err)
	}
	return s, f
}

func TestOnlyMinecraftUnit(t *testing.T) {
	for _, unit := range []string{"admin.service", "minecraft", "minecraft.service; rm -rf ~", "", "../minecraft.service"} {
		if _, err := NewSystemd(unit, nil); err == nil {
			t.Errorf("NewSystemd(%q) succeeded", unit)
		}
	}
	if _, err := NewSystemd("minecraft.service", nil); err != nil {
		t.Errorf("NewSystemd(minecraft.service): %v", err)
	}
}

func TestActions(t *testing.T) {
	for _, a := range []Action{Start, Stop, Restart} {
		s, f := newFake(t, "ActiveState=active\nSubState=running\n")
		if err := s.Do(context.Background(), a); err != nil {
			t.Fatalf("%s: %v", a, err)
		}
		want := []string{"systemctl", "--user", "--no-block", string(a), "minecraft.service"}
		if got := f.lastCall(); !reflect.DeepEqual(got, want) {
			t.Errorf("%s ran %q, want %q", a, got, want)
		}
	}
}

func TestUnknownAction(t *testing.T) {
	s, f := newFake(t, "ActiveState=active\n")
	for _, a := range []Action{"kill", "", "start minecraft.service"} {
		if err := s.Do(context.Background(), a); err == nil {
			t.Errorf("Do(%q) succeeded", a)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("ran %q", f.calls)
	}
	if _, ok := ParseAction("restart"); !ok {
		t.Error("ParseAction(restart) failed")
	}
	if _, ok := ParseAction("Restart"); ok {
		t.Error("ParseAction(Restart) succeeded")
	}
}

func TestBusyWhileStopping(t *testing.T) {
	s, f := newFake(t, "ActiveState=deactivating\nSubState=stop-sigterm\n")
	if err := s.Do(context.Background(), Start); !errors.Is(err, ErrBusy) {
		t.Errorf("Start while stopping: %v, want ErrBusy", err)
	}
	for _, c := range f.calls {
		if c[2] == "--no-block" {
			t.Errorf("ran %q while the unit was stopping", c)
		}
	}
}

func TestBusyWhileActionRuns(t *testing.T) {
	s, f := newFake(t, "ActiveState=active\nSubState=running\n")
	f.block = make(chan struct{})
	done := make(chan error)
	go func() { done <- s.Do(context.Background(), Stop) }()

	// Wait until the first action is inside systemctl.
	for {
		f.mu.Lock()
		n := len(f.calls)
		f.mu.Unlock()
		if n == 2 {
			break
		}
	}
	if err := s.Do(context.Background(), Restart); !errors.Is(err, ErrBusy) {
		t.Errorf("second action: %v, want ErrBusy", err)
	}
	close(f.block)
	if err := <-done; err != nil {
		t.Errorf("first action: %v", err)
	}
	// Once it's done, the next one runs.
	f.mu.Lock()
	f.block = nil
	f.mu.Unlock()
	if err := s.Do(context.Background(), Start); err != nil {
		t.Errorf("action after the first finished: %v", err)
	}
}

func TestActionFails(t *testing.T) {
	s, f := newFake(t, "ActiveState=failed\nSubState=failed\n")
	f.fail = errors.New("exit status 1: Unit minecraft.service not found.")
	err := s.Do(context.Background(), Start)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("Do = %v, want systemctl's error", err)
	}
}

func TestState(t *testing.T) {
	s, _ := newFake(t, "ActiveState=inactive\nSubState=dead\n")
	st, err := s.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st != (State{Active: "inactive", Sub: "dead"}) || st.Busy() {
		t.Errorf("State = %+v, busy %v", st, st.Busy())
	}

	s, _ = newFake(t, "garbage")
	if _, err := s.State(context.Background()); err == nil {
		t.Error("State with no ActiveState: no error")
	}
}
