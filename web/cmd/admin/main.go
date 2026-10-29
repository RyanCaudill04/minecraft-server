// Command admin serves admin.ryancaudill.dev: the static page in -dir plus a
// JSON API about the server and its deploys. Cloudflare Access sits in
// front of it, and every request must also carry a valid Access token
// (see package access); everything here is read-only for now.
//
//	GET /api/status  active, sleeping, starting or offline, and who is online
//	GET /api/deploy  the release mc-agent has active
//	GET /healthz     "ok", without a token, for mc-agent's health check
//
// The Access application is read from the environment: ACCESS_TEAM_DOMAIN
// (e.g. myteam.cloudflareaccess.com) and ACCESS_AUD (its audience tag).
// Without them admin still starts, so a deploy's health check passes, but
// answers everything except /healthz with 503 until they're set. For local
// development, -dev-user skips the token check instead.
//
// Usage: admin -dir folder -shared folder [-apps folder] [-addr host:port] [-paper host:port] [-lazymc host:port] [-dev-user email]
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/access"
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

// protect puts auth in front of h, except for /healthz: mc-agent's health
// check has no token, and the route reveals nothing.
func protect(h http.Handler, auth func(http.Handler) http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	mux.Handle("/", auth(h))
	return mux
}

// authFromEnv returns the token check for the Access application named in
// the environment, or, if devUser is set, a stand-in that lets every
// request through as devUser. If the environment is incomplete, it
// returns a check that refuses everything with 503, and logs why.
func authFromEnv(devUser string) func(http.Handler) http.Handler {
	if devUser != "" {
		log.Printf("admin: WARNING: -dev-user is set, so every request runs as %s without a token", devUser)
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r.WithContext(access.WithEmail(r.Context(), devUser)))
			})
		}
	}
	var missing []string
	team, aud := os.Getenv("ACCESS_TEAM_DOMAIN"), os.Getenv("ACCESS_AUD")
	if team == "" {
		missing = append(missing, "ACCESS_TEAM_DOMAIN")
	}
	if aud == "" {
		missing = append(missing, "ACCESS_AUD")
	}
	if len(missing) > 0 {
		msg := fmt.Sprintf("admin isn't configured: set %s in ~/.config/minecraft/secrets.env (see deploy/secrets.example.env)", strings.Join(missing, " and "))
		log.Printf("admin: %s; refusing every request until then", msg)
		return func(http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, msg, http.StatusServiceUnavailable)
			})
		}
	}
	v := &access.Verifier{
		Issuer: "https://" + team,
		AUD:    aud,
		HTTP:   &http.Client{Timeout: 10 * time.Second},
	}
	return v.Middleware
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
	devUser := flag.String("dev-user", "", "skip the Access token check and act as this email (local development only)")
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
	auth := authFromEnv(*devUser)
	c := &mcstatus.Checker{PaperAddr: *paper, LazymcAddr: *lazymc, Timeout: *timeout, Ping: ping.Status}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           protect(newHandler(*dir, *sharedDir, *appsDir, c), auth),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("serving %s and the API on http://%s", *dir, *addr)
	log.Fatal(srv.ListenAndServe())
}
