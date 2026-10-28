// Command admin serves admin.ryancaudill.dev: the static page in -dir plus a
// JSON API about the server and its deploys. Cloudflare Access sits in
// front of it; everything here is read-only for now.
//
//	GET /api/status  active, sleeping, starting or offline, and who is online
//	GET /api/deploy  the release mc-agent has active
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
	"github.com/ryancaudill04/minecraft-server/web/internal/mcstatus"
	"github.com/ryancaudill04/minecraft-server/web/internal/ping"
	"github.com/ryancaudill04/minecraft-server/web/internal/site"
)

// Deploy is the JSON body of GET /api/deploy.
type Deploy struct {
	Active string `json:"active"` // release tag, or "" if none is active
}

// newHandler routes the API and serves the static page from dir and the
// shared files from sharedDir. Deploy state is read from appsDir, the
// folder mc-agent installs releases into.
func newHandler(dir, sharedDir, appsDir string, c *mcstatus.Checker) http.Handler {
	status := &site.Cache[mcstatus.Status]{MaxAge: 5 * time.Second, Load: c.Check}
	agent := &deploy.Deployer{Apps: appsDir}

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
		site.WriteJSON(w, "admin: deploy", Deploy{Active: active})
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
