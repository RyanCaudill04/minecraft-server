package main

// World backups: the archives mc-backup.sh writes, flagging them for safe
// keeping, deleting and downloading them, and starting a backup now.

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/ryancaudill04/minecraft-server/web/internal/backups"
	"github.com/ryancaudill04/minecraft-server/web/internal/control"
	"github.com/ryancaudill04/minecraft-server/web/internal/site"
)

// Backups is the JSON body of GET /api/backups.
type Backups struct {
	// KeepDays is how old a backup gets before the nightly cleanup
	// removes it, unless it's kept.
	KeepDays  int              `json:"keep_days"`
	Backups   []backups.Backup `json:"backups"` // newest first
	TotalSize int64            `json:"total_size"`
	Free      *uint64          `json:"free,omitempty"` // bytes free on the backup disk
	// Unit is mc-backup.service's state: activating while a backup runs,
	// failed if the last one failed.
	Unit *control.State `json:"unit,omitempty"`
}

// Keep is the JSON body of POST /api/backups/{name}/keep.
type Keep struct {
	Keep *bool `json:"keep"`
}

func (a *app) listBackups(w http.ResponseWriter, r *http.Request) {
	if a.backups == nil {
		writeError(w, http.StatusServiceUnavailable, "Backups aren't configured.")
		return
	}
	list, err := a.backups.List()
	if err != nil {
		log.Printf("admin: backups: %v", err)
		writeError(w, http.StatusServiceUnavailable, "Couldn't read the backup folder.")
		return
	}
	res := Backups{KeepDays: a.backups.KeepDays, Backups: list}
	for _, b := range list {
		res.TotalSize += b.Size
	}
	if free, ok := a.backups.Free(); ok {
		res.Free = &free
	}
	if a.backupUnit != nil {
		st, err := a.backupUnit.State(r.Context())
		a.backupErrs.log("admin: backup unit state", err)
		if err == nil {
			res.Unit = &st
		}
	}
	site.WriteJSON(w, "admin: backups", res)
}

// runBackup handles POST /api/backups/run: start mc-backup.service now.
func (a *app) runBackup(w http.ResponseWriter, r *http.Request, user string) {
	if a.backupUnit == nil {
		writeError(w, http.StatusServiceUnavailable, "Starting a backup isn't available.")
		return
	}
	log.Printf("admin: audit: %s: start %s", user, control.BackupUnit)
	err := a.backupUnit.Do(context.WithoutCancel(r.Context()), control.Start)
	switch {
	case errors.Is(err, control.ErrBusy):
		writeError(w, http.StatusConflict, "A backup is already running.")
	case err != nil:
		log.Printf("admin: backup: %v", err)
		writeError(w, http.StatusBadGateway, "systemd refused: see admin's log.")
	default:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"action": "backup"})
	}
}

// keepBackup handles POST /api/backups/{name}/keep with {"keep": bool}.
func (a *app) keepBackup(w http.ResponseWriter, r *http.Request, user string) {
	if a.backups == nil {
		writeError(w, http.StatusServiceUnavailable, "Backups aren't configured.")
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "Send JSON.")
		return
	}
	var k Keep
	if err := json.NewDecoder(r.Body).Decode(&k); err != nil || k.Keep == nil {
		writeError(w, http.StatusBadRequest, `Send {"keep": true} or {"keep": false}.`)
		return
	}
	name := r.PathValue("name")
	if !backups.ValidName(name) {
		writeError(w, http.StatusNotFound, "No such backup.")
		return
	}
	log.Printf("admin: audit: %s: backup %s keep=%v", user, name, *k.Keep)
	err := a.backups.SetKeep(name, *k.Keep)
	if !a.backupError(w, "keep", err) {
		site.WriteJSON(w, "admin: backup keep", map[string]any{"name": name, "keep": *k.Keep})
	}
}

// deleteBackup handles POST /api/backups/{name}/delete.
func (a *app) deleteBackup(w http.ResponseWriter, r *http.Request, user string) {
	if a.backups == nil {
		writeError(w, http.StatusServiceUnavailable, "Backups aren't configured.")
		return
	}
	name := r.PathValue("name")
	if !backups.ValidName(name) {
		writeError(w, http.StatusNotFound, "No such backup.")
		return
	}
	log.Printf("admin: audit: %s: delete backup %s", user, name)
	err := a.backups.Delete(name)
	if !a.backupError(w, "delete", err) {
		site.WriteJSON(w, "admin: backup delete", map[string]string{"deleted": name})
	}
}

// downloadBackup handles GET /api/backups/{name}: the archive itself. It
// holds server.properties, so only a verified user gets it.
func (a *app) downloadBackup(w http.ResponseWriter, r *http.Request, user string) {
	if a.backups == nil {
		writeError(w, http.StatusServiceUnavailable, "Backups aren't configured.")
		return
	}
	name := r.PathValue("name")
	if !backups.ValidName(name) {
		writeError(w, http.StatusNotFound, "No such backup.")
		return
	}
	f, err := a.backups.Open(name)
	if a.backupError(w, "download", err) {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		a.backupError(w, "download", err)
		return
	}
	// One audit line per download, not one per resumed range.
	if r.Header.Get("Range") == "" {
		log.Printf("admin: audit: %s: download backup %s", user, name)
	}
	// name matches backups.ValidName, so it needs no quoting.
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// backupError writes the reply for err, if there is one, and reports
// whether it did.
func (a *app) backupError(w http.ResponseWriter, what string, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, backups.ErrNotFound):
		writeError(w, http.StatusNotFound, "No such backup. It may have been cleaned up.")
	case errors.Is(err, backups.ErrKept):
		writeError(w, http.StatusConflict, "That backup is kept for safe keeping. Remove the flag first.")
	default:
		log.Printf("admin: backup %s: %v", what, err)
		writeError(w, http.StatusInternalServerError, "Couldn't "+what+" the backup: see admin's log.")
	}
	return true
}
