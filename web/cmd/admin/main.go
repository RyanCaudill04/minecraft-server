// Command admin serves admin.ryancaudill.dev: the static page in -dir plus a
// JSON API about the server and its deploys. Cloudflare Access sits in
// front of it; everything here is read-only for now.
//
//	GET /api/status  active, sleeping, starting or offline, and who is online
//	GET /api/deploy  the release mc-agent has active, what changed in it,
//	                 and when it was released and applied
//	GET /api/plugins the jars in the server's plugins folder, checked
//	                 against plugins/manifest.json
//
// Usage: admin -dir folder -shared folder [-apps folder] [-server folder] [-addr host:port] [-paper host:port] [-lazymc host:port]
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/deploy"
	"github.com/ryancaudill04/minecraft-server/web/internal/mcstatus"
	"github.com/ryancaudill04/minecraft-server/web/internal/ping"
	"github.com/ryancaudill04/minecraft-server/web/internal/plugins"
	"github.com/ryancaudill04/minecraft-server/web/internal/site"
)

// Deploy is the JSON body of GET /api/deploy. Releases built before notes
// were added have only active and applied_at.
type Deploy struct {
	Active     string          `json:"active"`        // release tag, or "" if none is active
	Kind       string          `json:"kind,omitzero"` // feature, fix or other
	Changes    []deploy.Change `json:"changes,omitzero"`
	ReleasedAt time.Time       `json:"released_at,omitzero"` // when CI built it
	AppliedAt  time.Time       `json:"applied_at,omitzero"`  // when mc-agent last switched to it
}

// Plugins is the JSON body of GET /api/plugins.
type Plugins struct {
	Manifest bool             `json:"manifest"` // false if plugins/manifest.json is missing
	Plugins  []plugins.Plugin `json:"plugins"`
}

// newHandler routes the API and serves the static page from dir and the
// shared files from sharedDir. Deploy state is read from appsDir, the
// folder mc-agent installs releases into, and plugins from serverDir.
func newHandler(dir, sharedDir, appsDir, serverDir string, c *mcstatus.Checker) http.Handler {
	status := &site.Cache[mcstatus.Status]{MaxAge: 5 * time.Second, Load: c.Check}
	agent := &deploy.Deployer{Apps: appsDir}
	// Hashing every jar takes a moment on the Pi, and plugins change only
	// on a restart, so the list is cached for a minute.
	pluginDir := filepath.Join(serverDir, "plugins")
	installed := &site.Cache[Plugins]{MaxAge: time.Minute, Load: func(context.Context) (Plugins, error) {
		m, err := plugins.LoadManifest(filepath.Join(pluginDir, "manifest.json"))
		if err != nil {
			return Plugins{}, err
		}
		list, err := plugins.List(pluginDir, m)
		return Plugins{Manifest: m != nil, Plugins: list}, err
	}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", site.JSON("admin: status", status))
	mux.HandleFunc("GET /api/deploy", func(w http.ResponseWriter, r *http.Request) {
		// One readlink, so it's read fresh on every request.
		active, err := agent.Current()
		if err != nil {
			log.Printf("admin: deploy: %v", err)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		res := Deploy{Active: active}
		// The notes and applied time only add detail, so a problem with
		// either still shows the active release.
		if n, err := agent.Notes(); err != nil {
			log.Printf("admin: deploy notes: %v", err)
		} else if n != nil && n.Tag == active {
			res.Kind, res.Changes, res.ReleasedAt = n.Kind, n.Changes, n.ReleasedAt
		}
		if a, err := agent.LastApplied(); err != nil {
			log.Printf("admin: deploy applied time: %v", err)
		} else if a != nil {
			res.AppliedAt = a.At
		}
		site.WriteJSON(w, "admin: deploy", res)
	})
	mux.HandleFunc("GET /api/plugins", site.JSON("admin: plugins", installed))
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
	serverDir := flag.String("server", filepath.Join(home, "minecraft_server"), "Minecraft server folder, holding plugins/")
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
		Handler:           newHandler(*dir, *sharedDir, *appsDir, *serverDir, c),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("serving %s and the API on http://%s", *dir, *addr)
	log.Fatal(srv.ListenAndServe())
}
