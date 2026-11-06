package backups

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeFiles creates each file in dir with its name as its contents.
func writeFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func exists(dir, name string) bool {
	_, err := os.Lstat(filepath.Join(dir, name))
	return err == nil
}

func TestValidName(t *testing.T) {
	for name, want := range map[string]bool{
		"world-20261101-043000.tar.gz":         true,
		"my-world.v2-20261101-043000.tar.gz":   true,
		"world-20261101-043000.tar.gz.sha256":  false,
		"world-20261101-043000.tar.gz.partial": false,
		"world-20261101-043000.tar.gz.keep":    false,
		"world-old-copy.tar.gz":                false,
		"../world-20261101-043000.tar.gz":      false,
		"sub/world-20261101-043000.tar.gz":     false,
		".world-20261101-043000.tar.gz":        false,
		"":                                     false,
	} {
		if got := ValidName(name); got != want {
			t.Errorf("ValidName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestList(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir,
		"world-20261101-043000.tar.gz", "world-20261101-043000.tar.gz.sha256",
		"world-20261103-043000.tar.gz", "world-20261103-043000.tar.gz.sha256", "world-20261103-043000.tar.gz.keep",
		"world-20261102-120000.tar.gz",
		// Not backups: a partial, notes, an impossible date, a stray flag.
		"world-20261104-043000.tar.gz.partial", "notes.txt", "world-20261399-000000.tar.gz",
		"world-20261001-000000.tar.gz.keep",
	)
	if err := os.Mkdir(filepath.Join(dir, "world-20261105-043000.tar.gz"), 0o700); err != nil {
		t.Fatal(err)
	}

	s := &Store{Dir: dir, KeepDays: 10}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, b := range list {
		names = append(names, b.Name)
	}
	want := []string{"world-20261103-043000.tar.gz", "world-20261102-120000.tar.gz", "world-20261101-043000.tar.gz"}
	if len(names) != len(want) {
		t.Fatalf("listed %q, want %q", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("listed %q, want %q", names, want)
		}
	}

	kept, plain := list[0], list[2]
	if !kept.Keep || !kept.Expires.IsZero() || !kept.Checksum {
		t.Errorf("kept backup = %+v, want Keep, a checksum and no expiry", kept)
	}
	at := time.Date(2026, 11, 1, 4, 30, 0, 0, time.Local)
	if !plain.Created.Equal(at) || plain.Keep || !plain.Checksum || plain.Size != int64(len(plain.Name)) {
		t.Errorf("plain backup = %+v", plain)
	}
	if want := time.Date(2026, 11, 11, 4, 30, 0, 0, time.Local); !plain.Expires.Equal(want) {
		t.Errorf("expires %v, want %v (10 days)", plain.Expires, want)
	}
	if list[1].Checksum {
		t.Errorf("%s has no .sha256 but Checksum is set", list[1].Name)
	}
}

func TestListMissingFolder(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "none")}
	list, err := s.List()
	if err != nil || list == nil || len(list) != 0 {
		t.Errorf("List() on a missing folder = %v, %v; want an empty list", list, err)
	}
}

func TestDefaultKeepDays(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "world-20261101-043000.tar.gz")
	list, err := (&Store{Dir: dir}).List()
	if err != nil {
		t.Fatal(err)
	}
	if want := list[0].Created.AddDate(0, 0, DefaultKeepDays); !list[0].Expires.Equal(want) {
		t.Errorf("expires %v, want %v", list[0].Expires, want)
	}
}

func TestKeepAndDelete(t *testing.T) {
	dir := t.TempDir()
	name := "world-20261101-043000.tar.gz"
	writeFiles(t, dir, name, name+".sha256", "world-20261102-043000.tar.gz")
	s := &Store{Dir: dir}

	if err := s.SetKeep(name, true); err != nil {
		t.Fatal(err)
	}
	if !exists(dir, name+".keep") {
		t.Fatal("SetKeep(true) wrote no .keep file")
	}
	// Twice is fine.
	if err := s.SetKeep(name, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(name); !errors.Is(err, ErrKept) {
		t.Fatalf("Delete of a kept backup = %v, want ErrKept", err)
	}
	if !exists(dir, name) {
		t.Fatal("Delete removed a kept backup")
	}

	if err := s.SetKeep(name, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetKeep(name, false); err != nil {
		t.Fatalf("SetKeep(false) twice: %v", err)
	}
	if exists(dir, name+".keep") {
		t.Fatal("SetKeep(false) left the .keep file")
	}
	if err := s.Delete(name); err != nil {
		t.Fatal(err)
	}
	if exists(dir, name) || exists(dir, name+".sha256") {
		t.Error("Delete left the archive or its checksum")
	}
	if !exists(dir, "world-20261102-043000.tar.gz") {
		t.Error("Delete removed another backup")
	}
	// No checksum is fine too.
	if err := s.Delete("world-20261102-043000.tar.gz"); err != nil {
		t.Errorf("Delete without a checksum: %v", err)
	}
}

func TestRefusesOtherFiles(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "backups")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, parent, "world-20261101-043000.tar.gz")
	writeFiles(t, dir, "notes.txt")
	// A link that looks like a backup but points out of the folder.
	if err := os.Symlink(filepath.Join(parent, "world-20261101-043000.tar.gz"), filepath.Join(dir, "world-20261102-043000.tar.gz")); err != nil {
		t.Fatal(err)
	}
	s := &Store{Dir: dir}
	for _, name := range []string{
		"notes.txt", "../world-20261101-043000.tar.gz", "world-20261102-043000.tar.gz",
		"world-20261109-043000.tar.gz", // not there
	} {
		if err := s.Delete(name); !errors.Is(err, ErrNotFound) {
			t.Errorf("Delete(%q) = %v, want ErrNotFound", name, err)
		}
		if err := s.SetKeep(name, true); !errors.Is(err, ErrNotFound) {
			t.Errorf("SetKeep(%q) = %v, want ErrNotFound", name, err)
		}
		if f, err := s.Open(name); !errors.Is(err, ErrNotFound) {
			if f != nil {
				f.Close()
			}
			t.Errorf("Open(%q) = %v, want ErrNotFound", name, err)
		}
	}
	if !exists(dir, "notes.txt") || !exists(parent, "world-20261101-043000.tar.gz") {
		t.Error("removed a file that isn't a backup")
	}
	if exists(dir, "notes.txt.keep") {
		t.Error("flagged a file that isn't a backup")
	}
	list, _ := s.List()
	if len(list) != 0 {
		t.Errorf("listed %v; links and other files aren't backups", list)
	}
}

func TestOpen(t *testing.T) {
	dir := t.TempDir()
	name := "world-20261101-043000.tar.gz"
	writeFiles(t, dir, name)
	f, err := (&Store{Dir: dir}).Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	if string(b) != name {
		t.Errorf("read %q", b)
	}
}

func TestFree(t *testing.T) {
	if _, ok := (&Store{Dir: t.TempDir()}).Free(); !ok {
		t.Skip("free space isn't readable on this system")
	}
	if _, ok := (&Store{Dir: filepath.Join(t.TempDir(), "none")}).Free(); ok {
		t.Error("Free on a missing folder reported ok")
	}
}
