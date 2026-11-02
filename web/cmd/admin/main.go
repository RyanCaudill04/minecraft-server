// Command admin serves admin.ryancaudill.dev: the static page in -dir plus a
// JSON API about the server and its deploys. Cloudflare Access sits in
// front of it; everything here is read-only for now.
//
//	GET /api/status  active, sleeping, starting or offline, and who is online
//	GET /api/deploy  the release mc-agent has active, what changed in it,
//	                 and when it was released and applied; once the Pi
//	                 runs one folder per track, the same for each track
//
// Usage: admin -dir folder -shared folder [-apps folder] [-addr host:port] [-paper host:port] [-lazymc host:port]
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/deploy"
	"github.com/ryancaudill04/minecraft-server/web/internal/deploy/state"
	"github.com/ryancaudill04/minecraft-server/web/internal/mcstatus"
	"github.com/ryancaudill04/minecraft-server/web/internal/ping"
	"github.com/ryancaudill04/minecraft-server/web/internal/site"
)

// Deploy is the JSON body of GET /api/deploy. Releases built before notes
// were added have only active and applied_at. On the single release, the
// top-level fields describe it and tracks is empty. With one folder per
// track, they describe admin's own track, and tracks lists every track.
type Deploy struct {
	Release
	Layout string    `json:"layout,omitzero"` // "tracks", or left out for the single release
	Tracks []Release `json:"tracks,omitzero"`
}

// Release is what one track (or the single release) has active.
type Release struct {
	Track      string          `json:"track,omitzero"` // "" for the single release
	Active     string          `json:"active"`         // release tag, or "" if none is active
	Kind       string          `json:"kind,omitzero"`  // feature, fix or other
	Changes    []deploy.Change `json:"changes,omitzero"`
	ReleasedAt time.Time       `json:"released_at,omitzero"` // when CI built it
	AppliedAt  time.Time       `json:"applied_at,omitzero"`  // when mc-agent last switched to it
}

// adminTrack is the track this program ships in.
const adminTrack = "admin"

// release reads what d has active, fresh on every request.
func release(d *deploy.Deployer, track string) (Release, error) {
	active, err := d.Current()
	if err != nil {
		return Release{}, err
	}
	res := Release{Track: track, Active: active}
	// The notes and applied time only add detail, so a problem with
	// either still shows the active release.
	if n, err := d.Notes(); err != nil {
		log.Printf("admin: deploy notes: %v", err)
	} else if n != nil && n.Tag == active {
		res.Kind, res.Changes, res.ReleasedAt = n.Kind, n.Changes, n.ReleasedAt
	}
	if a, err := d.LastApplied(); err != nil {
		log.Printf("admin: deploy applied time: %v", err)
	} else if a != nil {
		res.AppliedAt = a.At
	}
	return res, nil
}

// readDeploy reads the deploy state in appsDir.
func readDeploy(appsDir string) (*Deploy, error) {
	st, err := state.Read(appsDir)
	if err != nil {
		return nil, err // a newer agent's format: says to update admin
	}
	agent := &deploy.Agent{Apps: appsDir}
	res := &Deploy{}
	if st.Layout != state.Tracks {
		d, _ := agent.TrackDeployer("")
		res.Release, err = release(d, "")
		return res, err
	}
	res.Layout = string(state.Tracks)
	for _, track := range st.Tracks {
		d, err := agent.TrackDeployer(track)
		if err != nil {
			return nil, err
		}
		r, err := release(d, track)
		if err != nil {
			return nil, err
		}
		res.Tracks = append(res.Tracks, r)
		if track == adminTrack {
			res.Release = r
		}
	}
	return res, nil
}

// newHandler routes the API and serves the static page from dir and the
// shared files from sharedDir. Deploy state is read from appsDir, the
// folder mc-agent installs releases into.
func newHandler(dir, sharedDir, appsDir string, c *mcstatus.Checker) http.Handler {
	status := &site.Cache[mcstatus.Status]{MaxAge: 5 * time.Second, Load: c.Check}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", site.JSON("admin: status", status))
	mux.HandleFunc("GET /api/deploy", func(w http.ResponseWriter, r *http.Request) {
		res, err := readDeploy(appsDir)
		if err != nil {
			log.Printf("admin: deploy: %v", err)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		site.WriteJSON(w, "admin: deploy", res)
	})
	mux.Handle("GET /", site.Static(dir, sharedDir))
	return mux
}

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("admin: %v", err)
	}
	dir := flag.String("dir", "", "folder with the page's static files (required)")
	sharedDir := flag.String("shared", "", "folder with the files all sites share, served at /shared/ (required)")
	appsDir := flag.String("apps", filepath.Join(home, "apps"), "folder mc-agent installs releases into")
	addr := flag.String("addr", "127.0.0.1:8081", "host:port to listen on")
	paper := flag.String("paper", "127.0.0.1:25566", "Paper's address (answers only while awake)")
	lazymc := flag.String("lazymc", "127.0.0.1:25565", "LazyMC's address (answers while Paper sleeps)")
	timeout := flag.Duration("timeout", 2*time.Second, "how long to wait for each ping")
	flag.Parse()

	if *dir == "" || *sharedDir == "" {
		fmt.Fprintln(os.Stderr, "admin: set -dir and -shared (e.g. -dir sites/admin -shared sites/shared)")
		os.Exit(2)
	}
	for _, d := range []string{*dir, *sharedDir} {
		info, err := os.Stat(d)
		if err != nil {
			log.Fatalf("admin: %v", err)
		}
		if !info.IsDir() {
			log.Fatalf("admin: %s is not a directory", d)
		}
	}
	c := &mcstatus.Checker{PaperAddr: *paper, LazymcAddr: *lazymc, Timeout: *timeout, Ping: ping.Status}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           newHandler(*dir, *sharedDir, *appsDir, c),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("serving %s and the API on http://%s", *dir, *addr)
	log.Fatal(srv.ListenAndServe())
}
