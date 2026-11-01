// Package activity turns the Minecraft server's log into a feed of joins,
// leaves, deaths and advancements.
//
// Paper writes logs/latest.log and rolls it over at midnight and on every
// start into logs/YYYY-MM-DD-N.log.gz. A Feed reads the newest rolled logs
// once for history, then follows latest.log from where it left off, across
// rollovers. Memory stays bounded: it keeps the last Max events and never
// holds more than one line of unparsed text.
//
// The feed only shows players who are visible in the server list. Players
// who hide themselves (the "Allow Server Listings" option) show up there as
// anonymous, so a Feed learns who is visible from status pings (Observe)
// and leaves everyone else out, including from other players' death
// messages.
package activity

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Event is one thing that happened in the game.
type Event struct {
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"` // Join, Leave, Death or Advancement
	Player string    `json:"player"`
	Text   string    `json:"text,omitempty"`  // the death message
	Title  string    `json:"title,omitempty"` // the advancement's title
}

// Limits on what a Feed reads and keeps.
const (
	defaultMax  = 100      // events kept
	maxLine     = 16 << 10 // longer lines are skipped
	maxRead     = 16 << 20 // bytes of log read per file per update
	maxBackfill = 3        // rolled logs read at the start
	maxNames    = 1024     // distinct player names remembered
)

// Feed follows a server's logs. The zero value isn't usable; set Dir.
type Feed struct {
	Dir       string // the server's logs folder
	StatePath string // where to keep the visible names; "" keeps them in memory
	Max       int    // events kept; 0 means 100

	mu      sync.Mutex
	loaded  bool
	file    *os.File // latest.log, kept open so a rollover can't lose its last lines
	partial []byte   // the start of a line not yet ended
	skip    bool     // inside a line longer than maxLine
	events  []Event
	online  map[string]bool // who the log says is on
	seen    map[string]bool // everyone who joined, to keep hidden names out of messages
	visible map[string]bool // names seen in the server list
	since   time.Time       // when the running server started
	running bool
}

// Update reads what the server logged since the last call. The first call
// also reads the newest rolled logs and the saved visible names.
func (f *Feed) Update() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.loaded {
		f.loaded = true
		f.online, f.seen, f.visible = map[string]bool{}, map[string]bool{}, map[string]bool{}
		if err := f.loadVisible(); err != nil {
			return err
		}
		f.backfill()
	}

	path := filepath.Join(f.Dir, "latest.log")
	if f.file != nil {
		// Finish the open file first: after a rollover it holds the last
		// lines of the old log under its new name.
		if err := f.follow(); err != nil {
			return err
		}
		cur, err1 := f.file.Stat()
		now, err2 := os.Stat(path)
		if err1 == nil && err2 == nil && os.SameFile(cur, now) {
			return nil
		}
		if err2 != nil && !errors.Is(err2, fs.ErrNotExist) {
			return fmt.Errorf("activity: %w", err2)
		}
		if errors.Is(err2, fs.ErrNotExist) {
			return nil // mid-rollover; the new file comes next time
		}
		f.closeFile()
	}

	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("activity: %w", err)
	}
	f.file = file
	// Start at most maxRead from the end, at a line boundary.
	if info, err := file.Stat(); err == nil && info.Size() > maxRead {
		if _, err := file.Seek(info.Size()-maxRead, io.SeekStart); err == nil {
			f.skip = true
		}
	}
	return f.follow()
}

// Close closes the log file the Feed holds open.
func (f *Feed) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeFile()
	return nil
}

func (f *Feed) closeFile() {
	if f.file != nil {
		f.file.Close()
		f.file = nil
	}
	f.partial, f.skip = nil, false
}

// follow reads the open log from its current offset to the end. A file
// that shrank was truncated in place, so it's read again from the start.
func (f *Feed) follow() error {
	info, err := f.file.Stat()
	if err != nil {
		return fmt.Errorf("activity: %w", err)
	}
	if pos, err := f.file.Seek(0, io.SeekCurrent); err == nil && info.Size() < pos {
		if _, err := f.file.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("activity: %w", err)
		}
		f.partial, f.skip = nil, false
	}

	b := batch{feed: f}
	if err := b.read(io.LimitReader(f.file, maxRead), false); err != nil {
		return fmt.Errorf("activity: %w", err)
	}
	// Lines are dated from the file's modification time: Paper writes as
	// things happen, so the last line was written then.
	if info, err := f.file.Stat(); err == nil {
		b.finish(info.ModTime())
	}
	return nil
}

// rolled matches the names Paper gives rolled logs, e.g. 2026-10-31-2.log.gz.
var rolled = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})-(\d{1,6})\.log\.gz$`)

// backfill reads the newest rolled logs, oldest first, for history from
// before latest.log began. Unreadable files are skipped.
func (f *Feed) backfill() {
	entries, err := os.ReadDir(f.Dir)
	if err != nil {
		return
	}
	type log struct {
		name string
		day  time.Time
		n    int
	}
	var logs []log
	for _, e := range entries {
		m := rolled.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() {
			continue
		}
		day, err := time.ParseInLocation(time.DateOnly, m[1], time.Local)
		if err != nil {
			continue
		}
		n, _ := strconv.Atoi(m[2])
		logs = append(logs, log{e.Name(), day, n})
	}
	slices.SortFunc(logs, func(a, b log) int {
		if c := a.day.Compare(b.day); c != 0 {
			return c
		}
		return a.n - b.n
	})
	for _, l := range logs[max(0, len(logs)-maxBackfill):] {
		// A rolled log is named for the day its lines were written.
		end := l.day.AddDate(0, 0, 1).Add(-time.Second)
		_ = f.readRolled(filepath.Join(f.Dir, l.name), end)
	}
}

func (f *Feed) readRolled(path string, end time.Time) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gz.Close()

	b := batch{feed: f}
	err = b.read(io.LimitReader(gz, maxRead), true)
	b.finish(end)
	return err
}

// batch is a run of log lines read together. Logs only stamp the time of
// day, so dates are worked out once the whole run is read, counting back
// from the last line, which was written at a known time.
type batch struct {
	feed    *Feed
	pending []pending
	days    int // midnights crossed so far
	last    int // clock of the last stamped line, or -1
	started bool
}

type pending struct {
	line
	day int // midnights crossed before this line
}

// read parses lines from r. With whole set, r is a complete file, so text
// after the last newline is a line too; otherwise it's kept for next time.
func (b *batch) read(r io.Reader, whole bool) error {
	f := b.feed
	if !b.started {
		b.started, b.last = true, -1
	}
	partial, skip := []byte(nil), false
	if !whole {
		partial, skip = f.partial, f.skip
	}

	br := bufio.NewReaderSize(r, 32<<10)
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 {
			ended := chunk[len(chunk)-1] == '\n'
			switch {
			case skip:
				// Dropping an overlong line until it ends.
			case len(partial)+len(chunk) > maxLine:
				partial, skip = nil, true
			default:
				partial = append(partial, chunk...)
			}
			if ended {
				if !skip {
					b.add(strings.TrimRight(string(partial), "\r\n"))
				}
				partial, skip = partial[:0], false
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}

	if whole {
		if len(partial) > 0 && !skip {
			b.add(string(partial))
		}
		return nil
	}
	f.partial, f.skip = slices.Clone(partial), skip
	return nil
}

// add handles one complete line: who's online changes right away, and
// events wait in pending until finish dates them.
func (b *batch) add(s string) {
	l, ok := parseLine(s)
	if !ok {
		return
	}
	// A clock that jumps back by more than 12 hours crossed midnight.
	if b.last >= 0 && l.clock < b.last-12*3600 {
		b.days++
	}
	b.last = l.clock

	f := b.feed
	switch l.kind {
	case "":
		return
	case started:
		clear(f.online)
		f.running = true
	case stopped:
		clear(f.online)
		f.running = false
		return
	case Join:
		f.online[l.name] = true
		if len(f.seen) < maxNames {
			f.seen[l.name] = true
		}
	case Leave:
		if !f.online[l.name] {
			return
		}
		delete(f.online, l.name)
	case Death, Advancement:
		// Only players the log has seen join. Paper logs named pets'
		// deaths and other look-alikes, which this leaves out.
		if !f.online[l.name] {
			return
		}
	}
	b.pending = append(b.pending, pending{l, b.days})
}

// finish dates the pending lines, given that the last stamped line was
// written at (or just before) end, and adds them to the feed.
func (b *batch) finish(end time.Time) {
	if b.last < 0 || len(b.pending) == 0 {
		return
	}
	f := b.feed
	end = end.In(time.Local)
	lastDay := time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, time.Local)
	if atClock(lastDay, b.last).After(end.Add(time.Minute)) {
		lastDay = lastDay.AddDate(0, 0, -1) // written just before midnight
	}

	for _, p := range b.pending {
		t := atClock(lastDay.AddDate(0, 0, p.day-b.days), p.clock)
		if p.kind == started {
			f.since = t
			continue
		}
		f.events = append(f.events, Event{Time: t.UTC(), Kind: p.kind, Player: p.name, Text: p.text, Title: p.title})
	}
	if limit := f.max(); len(f.events) > limit {
		f.events = slices.Clone(f.events[len(f.events)-limit:])
	}
	b.pending = b.pending[:0]
}

func atClock(day time.Time, clock int) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), clock/3600, clock/60%60, clock%60, 0, time.Local)
}

func (f *Feed) max() int {
	if f.Max > 0 {
		return f.Max
	}
	return defaultMax
}

// Observe records who the server list shows by name. With anonymous set,
// the list also had hidden players, so anyone the log says is online but
// the list doesn't name is treated as hidden from then on.
func (f *Feed) Observe(names []string, anonymous bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.loaded {
		return nil // Update hasn't run, so there's nothing to compare with
	}

	listed := map[string]bool{}
	changed := false
	for _, n := range names {
		if !playerName.MatchString(n) {
			continue
		}
		listed[n] = true
		if !f.visible[n] && len(f.visible) < maxNames {
			f.visible[n] = true
			changed = true
		}
	}
	if anonymous {
		for n := range f.online {
			if !listed[n] && f.visible[n] {
				delete(f.visible, n)
				changed = true
			}
		}
	}
	if changed {
		return f.saveVisible()
	}
	return nil
}

// Events returns up to n events by visible players, newest first.
func (f *Feed) Events(n int) []Event {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := []Event{}
	for i := len(f.events) - 1; i >= 0 && len(out) < n; i-- {
		e := f.events[i]
		if !f.visible[e.Player] || f.mentionsHidden(e.Text) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// mentionsHidden reports whether text names a player who isn't visible,
// such as a hidden player who killed a visible one.
func (f *Feed) mentionsHidden(text string) bool {
	for _, word := range strings.FieldsFunc(text, func(r rune) bool {
		return !(r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	}) {
		for _, w := range []string{word, strings.Trim(word, ".")} {
			if f.seen[w] && !f.visible[w] {
				return true
			}
		}
	}
	return false
}

// Running reports when the server started, if the log says it's running.
func (f *Feed) Running() (since time.Time, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.since, f.running && !f.since.IsZero()
}

// loadVisible reads the names saved by saveVisible, so visible players'
// history shows after a restart without waiting for them to log in.
func (f *Feed) loadVisible() error {
	if f.StatePath == "" {
		return nil
	}
	data, err := os.ReadFile(f.StatePath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("activity: %w", err)
	}
	var names []string
	if err := json.Unmarshal(data, &names); err != nil {
		return nil // a damaged file only costs the history until they're seen again
	}
	for _, n := range names {
		if playerName.MatchString(n) && len(f.visible) < maxNames {
			f.visible[n] = true
		}
	}
	return nil
}

// saveVisible writes the visible names, replacing the file atomically.
func (f *Feed) saveVisible() error {
	if f.StatePath == "" {
		return nil
	}
	names := make([]string, 0, len(f.visible))
	for n := range f.visible {
		names = append(names, n)
	}
	slices.Sort(names)
	data, err := json.Marshal(names)
	if err != nil {
		return err
	}
	tmp := f.StatePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("activity: %w", err)
	}
	if err := os.Rename(tmp, f.StatePath); err != nil {
		return fmt.Errorf("activity: %w", err)
	}
	return nil
}
