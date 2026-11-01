package activity

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Made-up players only: the repo is public.

// day is the date the test logs were written on, in the local zone the
// feed dates lines in.
var day = time.Date(2026, 10, 30, 0, 0, 0, 0, time.Local)

func at(t *testing.T, clock string) time.Time {
	t.Helper()
	c, err := time.ParseInLocation("15:04:05", clock, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	return time.Date(day.Year(), day.Month(), day.Day(), c.Hour(), c.Minute(), c.Second(), 0, time.Local)
}

// logLine formats a line as Paper's file log does.
func logLine(clock, msg string) string {
	return fmt.Sprintf("[%s] [Server thread/INFO]: %s\n", clock, msg)
}

func session(lines ...string) string {
	var b strings.Builder
	b.WriteString("[09:00:00] [main/INFO]: Environment: Environment[sessionHost=https://sessionserver.mojang.com]\n")
	b.WriteString(logLine("09:00:05", `Done (5.123s)! For help, type "help"`))
	for _, l := range lines {
		b.WriteString(l)
	}
	return b.String()
}

// writeLog writes latest.log in dir and sets its modification time.
func writeLog(t *testing.T, dir, content string, mtime time.Time) {
	t.Helper()
	path := filepath.Join(dir, "latest.log")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func appendLog(t *testing.T, dir, content string, mtime time.Time) {
	t.Helper()
	path := filepath.Join(dir, "latest.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func newFeed(t *testing.T, dir string) *Feed {
	t.Helper()
	f := &Feed{Dir: dir}
	t.Cleanup(func() { f.Close() })
	return f
}

func update(t *testing.T, f *Feed) {
	t.Helper()
	if err := f.Update(); err != nil {
		t.Fatal(err)
	}
}

func observe(t *testing.T, f *Feed, anonymous bool, names ...string) {
	t.Helper()
	if err := f.Observe(names, anonymous); err != nil {
		t.Fatal(err)
	}
}

// summary is a compact form of events for comparing.
func summary(events []Event) string {
	var parts []string
	for _, e := range events {
		s := e.Time.In(time.Local).Format("01-02 15:04:05") + " " + e.Kind + " " + e.Player
		if e.Text != "" {
			s += " (" + e.Text + ")"
		}
		if e.Title != "" {
			s += " [" + e.Title + "]"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n")
}

func checkEvents(t *testing.T, f *Feed, want ...string) {
	t.Helper()
	got := summary(f.Events(100))
	if w := strings.Join(want, "\n"); got != w {
		t.Errorf("events:\n%s\nwant:\n%s", got, w)
	}
}

func TestFeedReadsASession(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, session(
		logLine("10:00:00", "Alice joined the game"),
		"[10:00:00] [Server thread/INFO]: Alice[/127.0.0.1:50000] logged in with entity id 7 at ([world]0.5, 64.0, 0.5)\n",
		"[10:00:30] [Async Chat Thread - #0/INFO]: <Alice> hi\n",
		logLine("10:05:00", "Alice has made the advancement [Stone Age]"),
		logLine("10:10:00", "Alice was slain by Zombie"),
		logLine("10:10:01", "Rex was slain by Zombie"), // a named pet, not a player
		logLine("10:20:00", "Alice lost connection: Disconnected"),
		logLine("10:20:00", "Alice left the game"),
	), at(t, "10:20:00"))

	f := newFeed(t, dir)
	update(t, f)
	checkEvents(t, f) // nobody is known to be visible yet
	observe(t, f, false, "Alice")
	checkEvents(t, f,
		"10-30 10:20:00 leave Alice",
		"10-30 10:10:00 death Alice (Alice was slain by Zombie)",
		"10-30 10:05:00 advancement Alice [Stone Age]",
		"10-30 10:00:00 join Alice",
	)
	if since, ok := f.Running(); !ok || !since.Equal(at(t, "09:00:05")) {
		t.Errorf("Running() = %v, %v; want 09:00:05, true", since, ok)
	}
}

func TestFeedHidesHiddenPlayers(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, session(
		logLine("10:00:00", "Alice joined the game"),
		logLine("10:01:00", "Bob joined the game"),
		logLine("10:02:00", "Bob has made the advancement [Getting Wood]"),
		logLine("10:03:00", "Alice was slain by Bob"),
		logLine("10:03:30", "Alice was slain by Bobby"), // a different name
	), at(t, "10:03:30"))

	f := newFeed(t, dir)
	update(t, f)
	// Paper lists Bob as anonymous: he's hidden, and so is the death that
	// names him.
	observe(t, f, true, "Alice")
	checkEvents(t, f,
		"10-30 10:03:30 death Alice (Alice was slain by Bobby)",
		"10-30 10:00:00 join Alice",
	)

	// A player who turns listing back on shows up by name.
	observe(t, f, false, "Alice", "Bob")
	if got := len(f.Events(100)); got != 5 {
		t.Errorf("got %d events after Bob is listed; want 5", got)
	}

	// And one who turns it off again is hidden again.
	observe(t, f, true, "Alice")
	if got := len(f.Events(100)); got != 2 {
		t.Errorf("got %d events after Bob hides; want 2", got)
	}
}

func TestFeedFollowsAppends(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, session(logLine("10:00:00", "Alice joined the game")), at(t, "10:00:00"))
	f := newFeed(t, dir)
	update(t, f)
	observe(t, f, false, "Alice")

	// A half-written last line waits for the rest.
	appendLog(t, dir, "[10:05:00] [Server thread/INFO]: Alice has made the adv", at(t, "10:05:00"))
	update(t, f)
	checkEvents(t, f, "10-30 10:00:00 join Alice")

	appendLog(t, dir, "ancement [Stone Age]\r\n"+logLine("10:06:00", "Alice drowned"), at(t, "10:06:00"))
	update(t, f)
	checkEvents(t, f,
		"10-30 10:06:00 death Alice (Alice drowned)",
		"10-30 10:05:00 advancement Alice [Stone Age]",
		"10-30 10:00:00 join Alice",
	)

	// Nothing new, nothing added.
	update(t, f)
	if got := len(f.Events(100)); got != 3 {
		t.Errorf("got %d events after an idle update; want 3", got)
	}
}

func TestFeedFollowsRollover(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, session(logLine("23:50:00", "Alice joined the game")), at(t, "23:50:00"))
	f := newFeed(t, dir)
	update(t, f)
	observe(t, f, false, "Alice")

	// Paper writes a last line, then rolls the log over at midnight. The
	// feed still reads that line from the old file.
	appendLog(t, dir, logLine("23:59:00", "Alice has made the advancement [Hot Stuff]"), at(t, "23:59:00"))
	if err := os.Rename(filepath.Join(dir, "latest.log"), filepath.Join(dir, "2026-10-30-1.log")); err != nil {
		t.Fatal(err)
	}
	update(t, f) // mid-rollover: no latest.log yet
	next := day.AddDate(0, 0, 1)
	writeLog(t, dir, logLine("00:01:00", "Alice was slain by Zombie"), next.Add(time.Minute))
	update(t, f)

	checkEvents(t, f,
		"10-31 00:01:00 death Alice (Alice was slain by Zombie)",
		"10-30 23:59:00 advancement Alice [Hot Stuff]",
		"10-30 23:50:00 join Alice",
	)
}

func TestFeedRereadsATruncatedLog(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, session(logLine("10:00:00", "Alice joined the game"), logLine("10:30:00", "Alice left the game")), at(t, "10:30:00"))
	f := newFeed(t, dir)
	update(t, f)
	observe(t, f, false, "Alice")

	// Rewritten in place, shorter than before.
	path := filepath.Join(dir, "latest.log")
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	appendLog(t, dir, logLine("11:00:00", "Alice joined the game"), at(t, "11:00:00"))
	update(t, f)
	checkEvents(t, f,
		"10-30 11:00:00 join Alice",
		"10-30 10:30:00 leave Alice",
		"10-30 10:00:00 join Alice",
	)
}

func TestFeedDatesAcrossMidnight(t *testing.T) {
	dir := t.TempDir()
	next := day.AddDate(0, 0, 1)
	writeLog(t, dir, session(
		logLine("23:58:00", "Alice joined the game"),
		logLine("00:02:00", "Alice left the game"),
	), next.Add(2*time.Minute))
	f := newFeed(t, dir)
	update(t, f)
	observe(t, f, false, "Alice")
	checkEvents(t, f,
		"10-31 00:02:00 leave Alice",
		"10-30 23:58:00 join Alice",
	)
}

func TestFeedSkipsOverlongLines(t *testing.T) {
	dir := t.TempDir()
	long := "[10:01:00] [Server thread/INFO]: Alice was slain by " + strings.Repeat("x", 2*maxLine) + "\n"
	writeLog(t, dir, session(logLine("10:00:00", "Alice joined the game"), long, logLine("10:02:00", "Alice drowned")), at(t, "10:02:00"))
	f := newFeed(t, dir)
	update(t, f)
	observe(t, f, false, "Alice")
	checkEvents(t, f,
		"10-30 10:02:00 death Alice (Alice drowned)",
		"10-30 10:00:00 join Alice",
	)
}

func TestFeedKeepsMaxEvents(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	for i := range 20 {
		lines = append(lines, logLine(fmt.Sprintf("10:%02d:00", i), "Alice joined the game"))
	}
	writeLog(t, dir, session(lines...), at(t, "10:19:00"))
	f := &Feed{Dir: dir, Max: 5}
	defer f.Close()
	update(t, f)
	observe(t, f, false, "Alice")
	events := f.Events(100)
	if len(events) != 5 || !events[0].Time.Equal(at(t, "10:19:00")) || !events[4].Time.Equal(at(t, "10:15:00")) {
		t.Errorf("events:\n%s\nwant the last 5 joins", summary(events))
	}
	if got := f.Events(2); len(got) != 2 {
		t.Errorf("Events(2) returned %d", len(got))
	}
}

func writeRolled(t *testing.T, dir, name, content string) {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte(content))
	zw.Close()
	if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFeedBackfillsFromRolledLogs(t *testing.T) {
	dir := t.TempDir()
	writeRolled(t, dir, "2026-10-25-1.log.gz", session(logLine("12:00:00", "Alice joined the game")))
	writeRolled(t, dir, "2026-10-28-1.log.gz", session(logLine("12:00:00", "Alice joined the game")))
	writeRolled(t, dir, "2026-10-28-2.log.gz", session(logLine("18:00:00", "Alice joined the game"), logLine("19:00:00", "Alice left the game")))
	writeRolled(t, dir, "2026-10-29-10.log.gz", session(logLine("08:00:00", "Alice joined the game")))
	writeRolled(t, dir, "2026-10-29-9.log.gz", session(logLine("07:00:00", "Alice joined the game"), logLine("07:30:00", "Alice left the game")))
	writeRolled(t, dir, "notes.log.gz", session(logLine("07:00:00", "Mallory joined the game")))
	if err := os.WriteFile(filepath.Join(dir, "2026-10-30-1.log.gz"), []byte("not gzip"), 0o644); err != nil {
		t.Fatal(err)
	}

	f := newFeed(t, dir) // no latest.log: the server hasn't started since
	update(t, f)
	observe(t, f, false, "Alice")
	// The newest three rolled logs, in order; the damaged one is skipped.
	checkEvents(t, f,
		"10-29 08:00:00 join Alice",
		"10-29 07:30:00 leave Alice",
		"10-29 07:00:00 join Alice",
	)
}

func TestFeedRemembersVisibleNames(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(t.TempDir(), "visible.json")
	writeLog(t, dir, session(logLine("10:00:00", "Alice joined the game"), logLine("10:01:00", "Bob joined the game")), at(t, "10:01:00"))

	f := &Feed{Dir: dir, StatePath: state}
	update(t, f)
	observe(t, f, true, "Alice")
	f.Close()

	// After a restart, with the server asleep, Alice's history still shows.
	g := &Feed{Dir: dir, StatePath: state}
	defer g.Close()
	update(t, g)
	checkEvents(t, g, "10-30 10:00:00 join Alice")

	data, err := os.ReadFile(state)
	if err != nil || string(data) != `["Alice"]` {
		t.Errorf("state file = %q, %v", data, err)
	}
}

func TestFeedWithoutLogs(t *testing.T) {
	f := newFeed(t, filepath.Join(t.TempDir(), "missing"))
	update(t, f)
	observe(t, f, false, "Alice")
	if got := f.Events(10); got == nil || len(got) != 0 {
		t.Errorf("Events = %#v; want an empty list", got)
	}
}
