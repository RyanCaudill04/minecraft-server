package main

import (
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/ryancaudill04/minecraft-server/web/internal/site"
)

// mapCSP lets squaremap's page load only its own files. It fetches tiles
// and shows them as blob: URLs, and Leaflet positions them with inline
// style attributes, so styles need 'unsafe-inline'; scripts don't. Only play's
// own pages may frame it, for the preview on the main page.
const mapCSP = "default-src 'self'; img-src 'self' data: blob:; style-src 'self' 'unsafe-inline'; " +
	"frame-ancestors 'self'; base-uri 'none'; form-action 'none'"

// noPlayers stands in for squaremap's tiles/players.json, so the map never
// shows where anyone is. Showing positions would need a filter by the
// players' server-list consent (#113) and a CSP that allows squaremap's
// player heads, so there is no switch for it yet.
const noPlayers = `{"players":[],"max":0}` + "\n"

// mapHandler serves squaremap's web folder (the page, its scripts and the
// rendered tiles) read-only. It expects paths with the /map prefix already
// stripped.
//
// The folder is opened with os.Root on each request, so nothing outside it
// can be read, not even through a symlink, and play works before squaremap
// has created it. Folders are never listed. Tiles are cached for 5 minutes
// because squaremap rewrites one only when its area changes; everything
// else is checked on every load, because squaremap replaces its page on
// each update.
//
// Until squaremap has written its page, /map/ itself answers 404 with the
// page in notRenderedPage, which says so.
func mapHandler(webDir, notRenderedPage string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", mapCSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")

		if strings.EqualFold(path.Clean(r.URL.Path), "/tiles/players.json") {
			h.Set("Content-Type", "application/json")
			h.Set("Cache-Control", "no-store")
			w.Write([]byte(noPlayers))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/tiles/") && path.Ext(r.URL.Path) == ".png" {
			h.Set("Cache-Control", "public, max-age=300")
		} else {
			h.Set("Cache-Control", "no-cache")
		}

		root, err := os.OpenRoot(webDir)
		if err != nil {
			notRendered(w, r, notRenderedPage)
			return
		}
		defer root.Close()
		if r.URL.Path == "/" {
			if _, err := root.Stat("index.html"); err != nil {
				notRendered(w, r, notRenderedPage)
				return
			}
		}
		http.FileServerFS(filesOnly{root.FS()}).ServeHTTP(w, r)
	})
}

// notRendered answers a request for the map before squaremap has written
// it. The map's page gets the styled page in page; anything else, such as
// the tiles/settings.json play's own page checks for, gets plain text.
func notRendered(w http.ResponseWriter, r *http.Request, page string) {
	const msg = "The map hasn't been rendered yet."
	w.Header().Del("Cache-Control")
	if r.URL.Path == "/" {
		site.NotFound(w, page, msg)
		return
	}
	http.Error(w, msg, http.StatusNotFound)
}

// filesOnly opens files but no folders other than the top one, which
// http.FileServerFS needs to find index.html. Every error is reported as
// not found: a path that escapes the root, a permission problem or a
// missing tile all answer 404 and say nothing about the disk.
type filesOnly struct{ fsys fs.FS }

func (f filesOnly) Open(name string) (fs.File, error) {
	file, err := f.fsys.Open(name)
	if err != nil {
		return nil, fs.ErrNotExist
	}
	if name == "." {
		// Without an index.html, FileServerFS would list the folder.
		if _, err := fs.Stat(f.fsys, "index.html"); err != nil {
			file.Close()
			return nil, fs.ErrNotExist
		}
		return file, nil
	}
	info, err := file.Stat()
	if err != nil || info.IsDir() || !info.Mode().IsRegular() {
		file.Close()
		return nil, fs.ErrNotExist
	}
	return file, nil
}
