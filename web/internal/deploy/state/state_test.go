package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRead(t *testing.T) {
	tests := []struct {
		name    string
		file    string // "" means no state.json
		want    *State
		wantErr error
	}{
		{
			name: "missing file is the single layout",
			want: &State{Layout: Single},
		},
		{
			name: "version 1",
			file: `{"version": 1, "layout": "tracks", "tracks": ["play", "agent"]}`,
			want: &State{Version: 1, Layout: Tracks, Tracks: []string{"play", "agent"}},
		},
		{
			name: "no layout is single",
			file: `{"version": 1}`,
			want: &State{Version: 1, Layout: Single},
		},
		{
			name:    "newer version",
			file:    `{"version": 2, "layout": "something-else"}`,
			wantErr: ErrUnsupported,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apps := t.TempDir()
			if tt.file != "" {
				if err := os.WriteFile(filepath.Join(apps, FileName), []byte(tt.file), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := Read(apps)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestNewerVersionMessage(t *testing.T) {
	apps := t.TempDir()
	os.WriteFile(filepath.Join(apps, FileName), []byte(`{"version": 7}`), 0o644)
	_, err := Read(apps)
	if err == nil || !strings.Contains(err.Error(), "unsupported state version 7") {
		t.Errorf("err = %v, want it to name the unsupported version", err)
	}
}

func TestRoundTrip(t *testing.T) {
	apps := t.TempDir()
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	want := State{
		Version: Version,
		Layout:  Tracks,
		Tracks:  []string{"play", "admin", "sites", "agent"},
		Migration: &Migration{
			Tags:  map[string]string{"play": "play-r1"},
			Error: "health check failed",
			At:    at,
		},
	}
	err := Update(apps, func(s *State) error {
		if s.Version != 0 || s.Layout != Single {
			t.Errorf("a new state is %+v, want version 0, single", s)
		}
		*s = want
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Read(apps)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("got %+v, want %+v", *got, want)
	}
}

func TestUpdateRefusesNewerFile(t *testing.T) {
	apps := t.TempDir()
	path := filepath.Join(apps, FileName)
	newer := `{"version": 2, "layout": "tracks"}`
	os.WriteFile(path, []byte(newer), 0o644)

	called := false
	err := Update(apps, func(s *State) error { called = true; return nil })
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
	if called {
		t.Error("change was called for a file it can't read")
	}
	if data, _ := os.ReadFile(path); string(data) != newer {
		t.Errorf("state.json was rewritten to %s", data)
	}
}

func TestUpdateKeepsUnknownFields(t *testing.T) {
	apps := t.TempDir()
	path := filepath.Join(apps, FileName)
	os.WriteFile(path, []byte(`{"version": 1, "layout": "tracks", "policies": {"play": "auto"}}`), 0o644)

	if err := Update(apps, func(s *State) error { s.Tracks = []string{"play"}; return nil }); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got["policies"], map[string]any{"play": "auto"}) {
		t.Errorf("policies = %v, want it kept: %s", got["policies"], data)
	}
	if !reflect.DeepEqual(got["tracks"], []any{"play"}) {
		t.Errorf("tracks = %v, want [play]", got["tracks"])
	}
}

func TestUpdateErrorWritesNothing(t *testing.T) {
	apps := t.TempDir()
	err := Update(apps, func(s *State) error { s.Layout = Tracks; return errors.New("no") })
	if err == nil {
		t.Fatal("want the change's error")
	}
	if _, err := os.Stat(filepath.Join(apps, FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state.json was written: %v", err)
	}
}

// Two writers each add their own entries; with the lock, neither loses
// the other's.
func TestConcurrentUpdates(t *testing.T) {
	apps := t.TempDir()
	const each = 50
	var wg sync.WaitGroup
	for _, writer := range []string{"agent", "admin"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				err := Update(apps, func(s *State) error {
					s.Tracks = append(s.Tracks, fmt.Sprintf("%s%d", writer, i))
					return nil
				})
				if err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	s, err := Read(apps)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Tracks) != 2*each {
		t.Errorf("%d entries, want %d: an update was lost", len(s.Tracks), 2*each)
	}
}
