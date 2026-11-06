// Package backups lists and manages the world backups that
// deploy/backup/mc-backup.sh writes: <level>-YYYYmmdd-HHMMSS.tar.gz, with a
// .sha256 file next to each.
//
// A backup is kept for safe keeping when a <name>.keep file sits next to
// it. mc-backup.sh removes every other backup once it is KeepDays old,
// counting from the time in its name (the Pi's local time), so Expires
// here matches what the script does.
//
// Names are checked against the archive pattern and every file is opened
// through an os.Root on the backup folder, so a name from a request can
// never reach a file outside it, or one that isn't a backup.
package backups

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

// DefaultKeepDays is how long a backup lasts unless it's kept. The script
// and admin both read BACKUP_KEEP_DAYS to change it.
const DefaultKeepDays = 10

// The parts of a backup's file name.
const (
	stampLayout = "20060102-150405"
	archiveExt  = ".tar.gz"
	keepExt     = ".keep"
	sumExt      = ".sha256"
)

// archiveName matches the archives mc-backup.sh writes; the level name
// follows its own check of level-name.
var archiveName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*-[0-9]{8}-[0-9]{6}\.tar\.gz$`)

// ErrNotFound means there's no backup with that name.
var ErrNotFound = errors.New("backups: no such backup")

// ErrKept means the backup is kept for safe keeping, so it can't be deleted
// until the flag is removed.
var ErrKept = errors.New("backups: backup is kept")

// Backup is one archive.
type Backup struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Created  time.Time `json:"created"`  // from the name, in the Pi's local time
	Keep     bool      `json:"keep"`     // flagged for safe keeping
	Checksum bool      `json:"checksum"` // has its .sha256 file
	// Expires is when the nightly cleanup may remove it; zero if it's kept.
	Expires time.Time `json:"expires,omitzero"`
}

// Store is the backup folder.
type Store struct {
	Dir      string
	KeepDays int
}

// ValidName reports whether name is a backup archive's file name.
func ValidName(name string) bool { return archiveName.MatchString(name) }

// created parses the time out of a valid name.
func created(name string) (time.Time, error) {
	base := strings.TrimSuffix(name, archiveExt)
	return time.ParseInLocation(stampLayout, base[len(base)-len(stampLayout):], time.Local)
}

// open opens the backup folder. A missing folder is ErrNotExist: no backup
// has run yet.
func (s *Store) open() (*os.Root, error) { return os.OpenRoot(s.Dir) }

// List returns every backup, newest first. A missing folder has none.
func (s *Store) List() ([]Backup, error) {
	root, err := s.open()
	if errors.Is(err, fs.ErrNotExist) {
		return []Backup{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}
	list := []Backup{}
	for _, e := range entries {
		if !e.Type().IsRegular() || !ValidName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // removed since ReadDir
		}
		at, err := created(e.Name())
		if err != nil {
			continue // matches the pattern but isn't a real date
		}
		b := Backup{
			Name:     e.Name(),
			Size:     info.Size(),
			Created:  at,
			Keep:     names[e.Name()+keepExt],
			Checksum: names[e.Name()+sumExt],
		}
		if !b.Keep {
			b.Expires = s.expires(at)
		}
		list = append(list, b)
	}
	slices.SortFunc(list, func(a, b Backup) int { return b.Created.Compare(a.Created) })
	return list, nil
}

// expires is when a backup made at t is old enough for the cleanup. The
// script counts days of wall-clock time, as AddDate does.
func (s *Store) expires(t time.Time) time.Time {
	days := s.KeepDays
	if days < 1 {
		days = DefaultKeepDays
	}
	return t.AddDate(0, 0, days)
}

// exists checks name and that its archive is there.
func (s *Store) exists(root *os.Root, name string) error {
	if !ValidName(name) {
		return ErrNotFound
	}
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return ErrNotFound
	}
	return nil
}

// SetKeep flags a backup for safe keeping, or removes the flag.
func (s *Store) SetKeep(name string, keep bool) error {
	root, err := s.open()
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	defer root.Close()
	if err := s.exists(root, name); err != nil {
		return err
	}
	if !keep {
		if err := root.Remove(name + keepExt); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	f, err := root.OpenFile(name+keepExt, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// Delete removes a backup and its checksum. A kept backup is refused with
// ErrKept.
func (s *Store) Delete(name string) error {
	root, err := s.open()
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	defer root.Close()
	if err := s.exists(root, name); err != nil {
		return err
	}
	if _, err := root.Lstat(name + keepExt); err == nil {
		return ErrKept
	}
	if err := root.Remove(name); err != nil {
		return err
	}
	if err := root.Remove(name + sumExt); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("backups: removed %s but not its checksum: %w", name, err)
	}
	return nil
}

// Open opens a backup's archive for reading. The caller closes it.
func (s *Store) Open(name string) (*os.File, error) {
	root, err := s.open()
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if err := s.exists(root, name); err != nil {
		return nil, err
	}
	return root.Open(name)
}

// Free returns the bytes free for an unprivileged user on the backup
// folder's disk, or false if that can't be read.
func (s *Store) Free() (uint64, bool) { return freeBytes(s.Dir) }
