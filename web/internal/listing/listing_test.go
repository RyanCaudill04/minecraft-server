package listing

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/mcstatus"
)

const (
	aliceID = "11111111-1111-1111-1111-111111111111"
	bobID   = "22222222-2222-2222-2222-222222222222"
)

var t0 = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

func observe(t *testing.T, r *Registry, at time.Time, players ...mcstatus.Player) {
	t.Helper()
	if err := r.Observe(players, at); err != nil {
		t.Fatal(err)
	}
}

func open(t *testing.T, path string) *Registry {
	t.Helper()
	r, err := Open(path, DefaultGrace)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

var (
	alice     = mcstatus.Player{Name: "Alice", ID: aliceID}
	anonymous = mcstatus.Player{Name: "Anonymous Player", ID: mcstatus.AnonymousID}
)

func TestNobodyIsPublicUntilListed(t *testing.T) {
	r := open(t, "")
	if r.Public(aliceID, time.Time{}) || r.Public(aliceID, t0) {
		t.Error("a player never listed is public")
	}
	observe(t, r, t0, anonymous)
	if r.Public(mcstatus.AnonymousID, time.Time{}) {
		t.Error("the anonymous UUID is public")
	}
}

func TestPublicRule(t *testing.T) {
	r := open(t, "")
	observe(t, r, t0, alice, anonymous)

	tests := []struct {
		name     string
		lastSeen time.Time
		want     bool
	}{
		{"no data file", time.Time{}, true},
		{"played before the listing", t0.Add(-time.Hour), true},
		{"logout save just after the last listing", t0.Add(DefaultGrace), true},
		{"played after, without being listed", t0.Add(DefaultGrace + time.Second), false},
		{"played days later", t0.Add(72 * time.Hour), false},
	}
	for _, tt := range tests {
		if got := r.Public(aliceID, tt.lastSeen); got != tt.want {
			t.Errorf("%s: Public = %v, want %v", tt.name, got, tt.want)
		}
	}
	if r.Public(bobID, time.Time{}) {
		t.Error("Bob was only ever anonymous but is public")
	}

	// Listed again: public again, totals included.
	observe(t, r, t0.Add(72*time.Hour), alice)
	if !r.Public(aliceID, t0.Add(72*time.Hour)) {
		t.Error("Alice is listed again but still hidden")
	}
}

func TestObserveIgnoresMalformedEntries(t *testing.T) {
	r := open(t, "")
	bad := []mcstatus.Player{
		{Name: "Bob", ID: ""},
		{Name: "Bob", ID: "Bob-id"},
		{Name: "Bob", ID: "22222222-2222-2222-2222-22222222222G"},
		{Name: "Bob", ID: "22222222-2222-2222-2222-2222222222222"},
		{Name: "Bob", ID: "22222222222222222222222222222222"},
		{Name: "", ID: bobID},
		{Name: "<img src=x>", ID: bobID},
		{Name: "§aBob", ID: bobID},
		{Name: "ThisNameIsWayTooLong", ID: bobID},
		{Name: "Anonymous Player", ID: bobID},
	}
	observe(t, r, t0, bad...)
	if r.Public(bobID, time.Time{}) {
		t.Error("a malformed entry made Bob public")
	}
}

func TestObserveCap(t *testing.T) {
	r := open(t, "")
	var many []mcstatus.Player
	for i := range MaxPlayers + 5 {
		many = append(many, mcstatus.Player{Name: fmt.Sprintf("p%d", i), ID: fmt.Sprintf("%08x-0000-0000-0000-000000000000", i+1)})
	}
	observe(t, r, t0, many...)
	if len(r.seen) != MaxPlayers {
		t.Errorf("remembers %d players, want %d", len(r.seen), MaxPlayers)
	}
}

func TestStateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "listed.json")
	r := open(t, path)
	observe(t, r, t0, alice, anonymous)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %v, want 0600", mode)
	}
	data, _ := os.ReadFile(path)
	if want := `{"` + aliceID + `":{"name":"Alice","listed":"2026-10-30T12:00:00Z"}}`; string(data) != want {
		t.Errorf("listed.json = %s\nwant %s", data, want)
	}

	// Still listed a few seconds later: not written again yet.
	observe(t, r, t0.Add(15*time.Second), alice)
	if again, _ := os.ReadFile(path); string(again) != string(data) {
		t.Error("rewrote the file for a 15s update")
	}
	// After a minute it is.
	observe(t, r, t0.Add(time.Minute), alice)
	if again, _ := os.ReadFile(path); string(again) == string(data) {
		t.Error("didn't save after a minute")
	}

	// A restart remembers Alice.
	r = open(t, path)
	if !r.Public(aliceID, t0.Add(time.Minute)) {
		t.Error("Alice is hidden after a restart")
	}
	if r.Public(bobID, time.Time{}) {
		t.Error("Bob is public after a restart")
	}
}

func TestDamagedStateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "listed.json")
	bad := `{"` + aliceID + `":{"name":"Alice","listed":"2026-10-30T12:00:00Z"},` +
		`"not-a-uuid":{"name":"Bob","listed":"2026-10-30T12:00:00Z"},` +
		`"` + mcstatus.AnonymousID + `":{"name":"Anon","listed":"2026-10-30T12:00:00Z"}}`
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	r := open(t, path)
	if len(r.seen) != 1 || !r.Public(aliceID, time.Time{}) {
		t.Errorf("seen = %v, want only Alice", r.seen)
	}

	if err := os.WriteFile(path, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Open(path, DefaultGrace)
	if err == nil {
		t.Error("Open accepted a damaged file")
	}
	if r == nil || r.Public(aliceID, time.Time{}) {
		t.Error("a damaged file should give an empty, usable registry")
	}
	observe(t, r, t0, alice) // and the next save replaces it
	if _, err := Open(path, DefaultGrace); err != nil {
		t.Errorf("file not replaced: %v", err)
	}
}
