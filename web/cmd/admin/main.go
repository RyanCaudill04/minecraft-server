// Command admin serves admin.ryancaudill.dev: the static page in -dir plus a
// JSON API about the server and its deploys, and the update policy.
// Cloudflare Access sits in front of it.
//
//	GET  /api/status           active, sleeping, starting or offline, and who is online
//	GET  /api/deploy           the release mc-agent has active, what changed in it,
//	                           and when it was released and applied
//	GET  /api/settings         the update policy, pause, and the newest release seen
//	GET  /api/releases         releases on GitHub and installed ones, and which can be pinned
//	POST /api/settings/{track} {"mode": "auto"|"notify"} or {"mode": "pinned", "tag": "r7"}
//	POST /api/pause            {"paused": true|false}
//
// The POSTs need a verified user. Until admin checks the Access token
// itself (#27, #87), the only way to get one is -dev-user for local
// development, so they refuse every request in production. They must also
// be same-origin and carry the X-Admin-Request: 1 header, and each one is
// logged with the user's email. admin never deploys: it writes the change
// to mc-agent's request file and starts mc-agent.service.
//
// Usage: admin -dir folder -shared folder [-apps folder] [-addr host:port] [-paper host:port] [-lazymc host:port] [-repo owner/name] [-api url] [-dev-user email]
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/deploy"
	"github.com/ryancaudill04/minecraft-server/web/internal/mcstatus"
	"github.com/ryancaudill04/minecraft-server/web/internal/ping"
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

// app is everything the handlers need.
type app struct {
	dir, sharedDir string
	checker        *mcstatus.Checker
	// agent reads mc-agent's state in the apps folder, and asks GitHub for
	// releases. admin only reads that state and submits requests.
	agent *deploy.Deployer
	repo  string // GitHub "owner/name", for release links
	// user returns the verified email of whoever sent r, or "" if there
	// isn't one. Only a verified user may change anything.
	user func(r *http.Request) string
	// startAgent starts mc-agent.service; replaced in tests.
	startAgent func(ctx context.Context) error

	github   *site.Cache[published]
	submitMu sync.Mutex
}

// newHandler routes the API and serves the static page from a.dir and the
// shared files from a.sharedDir.
func newHandler(a *app) http.Handler {
	if a.user == nil {
		a.user = func(*http.Request) string { return "" }
	}
	if a.startAgent == nil {
		a.startAgent = startAgent
	}
	// GitHub allows 60 unauthenticated requests an hour, shared with
	// mc-agent's timer, so ask it at most every 10 minutes.
	a.github = &site.Cache[published]{MaxAge: 10 * time.Minute, Load: func(ctx context.Context) (published, error) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		rels, err := a.agent.Releases(ctx)
		return published{rels, err}, nil
	}}
	status := &site.Cache[mcstatus.Status]{MaxAge: 5 * time.Second, Load: a.checker.Check}
	agent := a.agent

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/settings", a.settings)
	mux.HandleFunc("GET /api/releases", a.releases)
	mux.Handle("POST /api/settings/{track}", a.writes(a.setPolicy))
	mux.Handle("POST /api/pause", a.writes(a.setPaused))
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
	mux.Handle("GET /", site.Static(a.dir, a.sharedDir))
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
	repo := flag.String("repo", "RyanCaudill04/minecraft-server", "GitHub repo mc-agent deploys releases from")
	api := flag.String("api", "https://api.github.com", "GitHub API base URL")
	devUser := flag.String("dev-user", "", "treat every request as signed in as this email (local development only)")
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
	a := &app{
		dir: *dir, sharedDir: *sharedDir, repo: *repo,
		checker: &mcstatus.Checker{PaperAddr: *paper, LazymcAddr: *lazymc, Timeout: *timeout, Ping: ping.Status},
		agent: &deploy.Deployer{
			Repo: *repo, API: *api, Apps: *appsDir,
			HTTP: &http.Client{Timeout: 10 * time.Second},
			Log:  log.New(os.Stderr, "admin: ", 0),
		},
	}
	if *devUser != "" {
		log.Printf("admin: WARNING: -dev-user is set, so every request can change the update policy as %s", *devUser)
		a.user = func(*http.Request) string { return *devUser }
	} else {
		log.Printf("admin: release and policy changes are locked until admin verifies the Access token (#27)")
	}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           newHandler(a),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("serving %s and the API on http://%s", *dir, *addr)
	log.Fatal(srv.ListenAndServe())
}
