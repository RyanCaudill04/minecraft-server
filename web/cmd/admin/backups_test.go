package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ryancaudill04/minecraft-server/web/internal/control"
	"github.com/ryancaudill04/minecraft-server/web/internal/mcstatus"
)

const (
	oldBackup = "world-20261101-043000.tar.gz"
	newBackup = "world-20261104-043000.tar.gz"
)

// addBackups puts two fake archives, each with a checksum, in the folder.
func (s *controlServer) addBackups(t *testing.T) {
	t.Helper()
	for _, n := range []string{oldBackup, newBackup} {
		for _, f := range []string{n, n + ".sha256"} {
			if err := os.WriteFile(filepath.Join(s.backupDir, f), []byte("data of "+f), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func (s *controlServer) has(name string) bool {
	_, err := os.Stat(filepath.Join(s.backupDir, name))
	return err == nil
}

func TestListBackups(t *testing.T) {
	s := newControlServer(t, "ryan@example.com")
	s.addBackups(t)
	if err := os.WriteFile(filepath.Join(s.backupDir, newBackup+".keep"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	var got Backups
	if code := getJSON(t, s.URL+"/api/backups", &got); code != http.StatusOK {
		t.Fatalf("GET /api/backups = %d", code)
	}
	if got.KeepDays != 10 || len(got.Backups) != 2 {
		t.Fatalf("got %+v, want 10 days and 2 backups", got)
	}
	if got.Backups[0].Name != newBackup || !got.Backups[0].Keep || got.Backups[1].Keep {
		t.Errorf("backups = %+v, want the kept newest one first", got.Backups)
	}
	if want := got.Backups[0].Size + got.Backups[1].Size; got.TotalSize != want || want == 0 {
		t.Errorf("total = %d, want %d", got.TotalSize, want)
	}
	if got.Unit == nil || got.Unit.Active != "inactive" {
		t.Errorf("unit = %+v, want mc-backup.service's state", got.Unit)
	}

	var me Me
	getJSON(t, s.URL+"/api/me", &me)
	if !me.Backups {
		t.Errorf("/api/me = %+v, want backups on", me)
	}
}

func TestBackupsLockedWithoutUser(t *testing.T) {
	s := newControlServer(t, "")
	s.addBackups(t)

	for path, body := range map[string]string{
		"/api/backups/run":                      "",
		"/api/backups/" + oldBackup + "/keep":   `{"keep":true}`,
		"/api/backups/" + oldBackup + "/delete": "",
	} {
		if code, got := s.post(t, path, body, nil); code != http.StatusForbidden || got["error"] != lockedNoUser {
			t.Errorf("POST %s without a user = %d %v, want 403", path, code, got)
		}
	}
	resp, err := http.Get(s.URL + "/api/backups/" + oldBackup)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("download without a user = %d, want 403", resp.StatusCode)
	}
	if !s.has(oldBackup) || s.has(oldBackup+".keep") || len(s.backup.done()) != 0 {
		t.Error("changed backups without a user")
	}
}

func TestKeepAndDeleteBackup(t *testing.T) {
	s := newControlServer(t, "ryan@example.com")
	s.addBackups(t)
	keep := "/api/backups/" + oldBackup + "/keep"
	del := "/api/backups/" + oldBackup + "/delete"

	if code, got := s.post(t, keep, `{"keep":true}`, nil); code != http.StatusOK {
		t.Fatalf("keep = %d %v", code, got)
	}
	if !s.has(oldBackup + ".keep") {
		t.Fatal("keep wrote no flag")
	}
	// A kept backup can't be deleted.
	if code, got := s.post(t, del, "", nil); code != http.StatusConflict {
		t.Errorf("delete of a kept backup = %d %v, want 409", code, got)
	}
	if !s.has(oldBackup) {
		t.Fatal("deleted a kept backup")
	}

	if code, got := s.post(t, keep, `{"keep":false}`, nil); code != http.StatusOK {
		t.Fatalf("unkeep = %d %v", code, got)
	}
	if code, got := s.post(t, del, "", nil); code != http.StatusOK || got["deleted"] != oldBackup {
		t.Fatalf("delete = %d %v", code, got)
	}
	if s.has(oldBackup) || s.has(oldBackup+".sha256") {
		t.Error("delete left the archive or its checksum")
	}
	if !s.has(newBackup) {
		t.Error("delete removed the other backup")
	}
	if code, _ := s.post(t, del, "", nil); code != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", code)
	}
	for _, want := range []string{
		"audit: ryan@example.com: backup " + oldBackup + " keep=true",
		"audit: ryan@example.com: delete backup " + oldBackup,
	} {
		if !strings.Contains(s.out.String(), want) {
			t.Errorf("audit log lacks %q:\n%s", want, s.out)
		}
	}
}

func TestKeepNeedsABody(t *testing.T) {
	s := newControlServer(t, "ryan@example.com")
	s.addBackups(t)
	path := "/api/backups/" + oldBackup + "/keep"
	for _, body := range []string{"", "{}", `{"keep":"yes"}`, "not json"} {
		if code, _ := s.post(t, path, body, nil); code != http.StatusBadRequest {
			t.Errorf("keep with %q = %d, want 400", body, code)
		}
	}
	if code, _ := s.post(t, path, `{"keep":true}`, map[string]string{"Content-Type": "text/plain"}); code != http.StatusUnsupportedMediaType {
		t.Errorf("keep as text/plain = %d, want 415", code)
	}
	if s.has(oldBackup + ".keep") {
		t.Error("a bad request flagged the backup")
	}
}

func TestBackupNamesStayInTheFolder(t *testing.T) {
	s := newControlServer(t, "ryan@example.com")
	s.addBackups(t)
	outside := filepath.Join(filepath.Dir(s.backupDir), "world-20261102-043000.tar.gz")
	if err := os.WriteFile(outside, []byte("not a backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.backupDir, "notes.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"notes.txt", oldBackup + ".sha256", "..%2Fworld-20261102-043000.tar.gz",
		"world-20261109-043000.tar.gz",
	} {
		if code, _ := s.post(t, "/api/backups/"+name+"/delete", "", nil); code != http.StatusNotFound {
			t.Errorf("delete %s = %d, want 404", name, code)
		}
		resp, err := http.Get(s.URL + "/api/backups/" + name)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("download %s = %d, want 404", name, resp.StatusCode)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Error("removed a file outside the backup folder")
	}
	if !s.has("notes.txt") || !s.has(oldBackup+".sha256") {
		t.Error("removed a file that isn't a backup")
	}
}

func TestDownloadBackup(t *testing.T) {
	s := newControlServer(t, "ryan@example.com")
	s.addBackups(t)
	resp, err := http.Get(s.URL + "/api/backups/" + oldBackup)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "data of "+oldBackup {
		t.Fatalf("download = %d %q", resp.StatusCode, body)
	}
	if got, want := resp.Header.Get("Content-Disposition"), `attachment; filename="`+oldBackup+`"`; got != want {
		t.Errorf("Content-Disposition = %q, want %q", got, want)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if !strings.Contains(s.out.String(), "audit: ryan@example.com: download backup "+oldBackup) {
		t.Errorf("download wasn't audited:\n%s", s.out)
	}
}

func TestRunBackup(t *testing.T) {
	s := newControlServer(t, "ryan@example.com")
	code, got := s.post(t, "/api/backups/run", "", nil)
	if code != http.StatusAccepted || got["action"] != "backup" {
		t.Fatalf("run = %d %v, want 202", code, got)
	}
	if fmt.Sprint(s.backup.done()) != "[start]" {
		t.Errorf("backup unit got %v, want [start]", s.backup.done())
	}
	if len(s.ctl.done()) != 0 {
		t.Errorf("a backup touched minecraft.service: %v", s.ctl.done())
	}

	s.backup.err = control.ErrBusy
	if code, _ := s.post(t, "/api/backups/run", "", nil); code != http.StatusConflict {
		t.Errorf("run while one runs = %d, want 409", code)
	}
	// Cross-origin and header checks apply here too.
	s.backup.err = nil
	if code, _ := s.post(t, "/api/backups/run", "", map[string]string{CSRFHeader: ""}); code != http.StatusForbidden {
		t.Errorf("run without %s = %d, want 403", CSRFHeader, code)
	}
	if code, _ := s.post(t, "/api/backups/run", "", map[string]string{"Sec-Fetch-Site": "cross-site"}); code != http.StatusForbidden {
		t.Errorf("cross-site run = %d, want 403", code)
	}
	if fmt.Sprint(s.backup.done()) != "[start]" {
		t.Errorf("refused requests still started a backup: %v", s.backup.done())
	}
}

func TestBackupsOff(t *testing.T) {
	a := &app{
		dir: t.TempDir(), sharedDir: t.TempDir(), appsDir: t.TempDir(),
		checker: &mcstatus.Checker{PaperAddr: "paper", LazymcAddr: "lazymc", Ping: (&fakeServer{}).ping},
		user:    func(*http.Request) string { return "ryan@example.com" },
	}
	ts := httptest.NewServer(newHandler(a))
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/backups")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET /api/backups without a store = %d, want 503", resp.StatusCode)
	}
	var me Me
	getJSON(t, ts.URL+"/api/me", &me)
	if me.Backups {
		t.Errorf("/api/me = %+v, want backups off", me)
	}
}
