// Command admin serves admin.ryancaudill.dev: the static page in -dir plus a
// JSON API about the server and its deploys, and controls for the server.
// Cloudflare Access sits in front of it, and every request must also carry
// a valid Access token (see package access).
//
//	GET  /healthz             "ok", without a token, for mc-agent's health check
//	GET  /api/status          active, sleeping, starting or offline, who is
//	                          online, and minecraft.service's systemd state
//	GET  /api/deploy          the release mc-agent has active, what changed
//	                          in it, when it was released and applied, the
//	                          installed and failed releases, and the last
//	                          deploy's result
//	GET  /api/plugins         the jars in the server's plugins folder, checked
//	                          against plugins/manifest.json
//	GET  /api/me              who is signed in and which controls they can use
//	GET  /api/logs?lines=n    the end of Paper's latest.log
//	POST /api/console         run {"command": "..."} over RCON
//	POST /api/server/{action} start, stop or restart minecraft.service
//	GET  /api/backups         the world backups, newest first, and whether
//	                          one is running
//	GET  /api/backups/{name}  download one
//	POST /api/backups/run     start mc-backup.service now
//	POST /api/backups/{name}/keep    {"keep": true} flags it for safe keeping
//	POST /api/backups/{name}/delete  remove one that isn't kept
//	GET  /api/players         the whitelist and ops list, from the server
//	                          folder, and whether the whitelist is on
//	POST /api/players/{list}/{action}  {"name": "..."}: whitelist add or
//	                          remove, or ops add (op) or remove (deop),
//	                          over RCON
//
// Logs, the console, the controls, downloads, the player lists and every
// change need the user the Access token names. POSTs must also be
// same-origin and carry the X-Admin-Request: 1 header, and each one is
// logged with the user's email.
//
// The Access application is read from the environment: ACCESS_TEAM_DOMAIN
// (e.g. myteam.cloudflareaccess.com) and ACCESS_AUD (its audience tag).
// Without them admin still starts, so a deploy's health check passes, but
// answers everything except /healthz with 503 until they're set. For local
// development, -dev-user skips the token check instead.
//
// Backups are read from BACKUP_DIR (default ~/backups/minecraft), and
// BACKUP_KEEP_DAYS says how long the nightly cleanup keeps them (default
// 10), the same settings mc-backup.sh reads from host.env. -backups and
// -backup-keep-days override them.
//
// The RCON password is read from RCON_PASSWORD (set in
// ~/.config/minecraft/secrets.env, which admin.service loads), never from a
// flag. Without it the console is off.
//
// Usage: admin -dir folder -shared folder [-apps folder] [-server folder] [-addr host:port] [-paper host:port] [-lazymc host:port] [-rcon host:port] [-logs file] [-backups folder] [-backup-keep-days n] [-dev-user email]
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/access"
	"github.com/ryancaudill04/minecraft-server/web/internal/backups"
	"github.com/ryancaudill04/minecraft-server/web/internal/control"
	"github.com/ryancaudill04/minecraft-server/web/internal/deploy"
	"github.com/ryancaudill04/minecraft-server/web/internal/mcstatus"
	"github.com/ryancaudill04/minecraft-server/web/internal/ping"
	"github.com/ryancaudill04/minecraft-server/web/internal/plugins"
	"github.com/ryancaudill04/minecraft-server/web/internal/rcon"
	"github.com/ryancaudill04/minecraft-server/web/internal/site"
)

// Deploy is the JSON body of GET /api/deploy. Releases built before notes
// were added have no kind, changes or released_at.
type Deploy struct {
	Active     string          `json:"active"`        // release tag, or "" if none is active
	Kind       string          `json:"kind,omitzero"` // feature, fix or other
	Changes    []deploy.Change `json:"changes,omitzero"`
	ReleasedAt time.Time       `json:"released_at,omitzero"` // when CI built it
	AppliedAt  time.Time       `json:"applied_at,omitzero"`  // when mc-agent last switched to it
	Failed     string          `json:"failed,omitzero"`      // a release mc-agent won't retry
	Installed  []string        `json:"installed,omitzero"`   // newest first
	LastResult *deploy.Result  `json:"last_result,omitzero"` // the last deploy's outcome
}

// Plugins is the JSON body of GET /api/plugins.
type Plugins struct {
	Manifest bool             `json:"manifest"` // false if plugins/manifest.json is missing
	Plugins  []plugins.Plugin `json:"plugins"`
}

// Status is the JSON body of GET /api/status: the ping state, plus the
// unit's systemd state when it can be read.
type Status struct {
	mcstatus.Status
	Unit *control.State `json:"unit,omitempty"`
}

// app is everything the handlers need.
type app struct {
	dir, sharedDir, appsDir, serverDir string
	checker                            *mcstatus.Checker
	// control starts and stops the server; nil turns the controls off.
	control control.Controller
	// wake asks a sleeping LazyMC to start Paper; nil turns it off.
	wake func(ctx context.Context) error
	// rcon runs a console command; nil when there's no RCON password.
	rcon func(ctx context.Context, cmd string) (string, error)
	// backups is the backup folder; nil turns the Backups section off.
	backups *backups.Store
	// backupUnit starts mc-backup.service; nil turns that button off.
	backupUnit control.Controller
	// logPath is Paper's latest.log.
	logPath string
	// user returns the verified email of whoever sent r, or "" if there
	// isn't one. Only a verified user may use the controls. It defaults to
	// the email the Access check put on the request.
	user func(r *http.Request) string

	status     *site.Cache[Status]
	unitErrs   logOnce
	backupErrs logOnce
}

// newHandler routes the API and serves the static page from a.dir and the
// shared files from a.sharedDir. Deploy state is read from a.appsDir, the
// folder mc-agent installs releases into, and plugins from a.serverDir.
func newHandler(a *app) http.Handler {
	if a.user == nil {
		a.user = func(r *http.Request) string { return access.Email(r.Context()) }
	}
	a.status = &site.Cache[Status]{MaxAge: 5 * time.Second, Load: a.loadStatus}
	agent := &deploy.Deployer{Apps: a.appsDir}
	// Hashing every jar takes a moment on the Pi, and plugins change only
	// on a restart, so the list is cached for a minute.
	pluginDir := filepath.Join(a.serverDir, "plugins")
	installed := &site.Cache[Plugins]{MaxAge: time.Minute, Load: func(context.Context) (Plugins, error) {
		m, err := plugins.LoadManifest(filepath.Join(pluginDir, "manifest.json"))
		if err != nil {
			return Plugins{}, err
		}
		list, err := plugins.List(pluginDir, m)
		return Plugins{Manifest: m != nil, Plugins: list}, err
	}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", site.JSON("admin: status", a.status))
	mux.HandleFunc("GET /api/deploy", func(w http.ResponseWriter, r *http.Request) {
		// A few small files, so it's read fresh on every request. agent has
		// no API set, so this never calls GitHub.
		s, err := agent.Status(r.Context())
		if err != nil {
			log.Printf("admin: deploy: %v", err)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		for _, msg := range s.Warnings {
			log.Printf("admin: deploy: %s", msg)
		}
		res := Deploy{
			Active:     s.Active,
			AppliedAt:  s.AppliedAt,
			Failed:     s.Failed,
			Installed:  s.Installed,
			LastResult: s.LastResult,
		}
		// The notes only add detail, so a problem with them still shows
		// the active release.
		if n, err := agent.Notes(); err != nil {
			log.Printf("admin: deploy notes: %v", err)
		} else if n != nil && n.Tag == s.Active {
			res.Kind, res.Changes, res.ReleasedAt = n.Kind, n.Changes, n.ReleasedAt
		}
		site.WriteJSON(w, "admin: deploy", res)
	})
	mux.HandleFunc("GET /api/plugins", site.JSON("admin: plugins", installed))
	mux.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) {
		site.WriteJSON(w, "admin: me", a.me(r))
	})
	mux.HandleFunc("GET /api/logs", a.signedIn(a.logs))
	mux.Handle("POST /api/console", a.writes(a.console))
	mux.Handle("POST /api/server/wake", a.writes(a.wakeServer))
	mux.Handle("POST /api/server/{action}", a.writes(a.serverAction))
	mux.HandleFunc("GET /api/backups", a.listBackups)
	mux.HandleFunc("GET /api/backups/{name}", a.signedIn(a.downloadBackup))
	mux.Handle("POST /api/backups/run", a.writes(a.runBackup))
	mux.Handle("POST /api/backups/{name}/keep", a.writes(a.keepBackup))
	mux.Handle("POST /api/backups/{name}/delete", a.writes(a.deleteBackup))
	mux.HandleFunc("GET /api/players", a.signedIn(a.players))
	mux.Handle("POST /api/players/{list}/{action}", a.writes(a.changePlayer))
	mux.Handle("GET /", site.Static(a.dir, a.sharedDir))
	return mux
}

func (a *app) loadStatus(ctx context.Context) (Status, error) {
	s, err := a.checker.Check(ctx)
	if err != nil {
		return Status{}, err
	}
	res := Status{Status: s}
	if a.control != nil {
		st, err := a.control.State(ctx)
		a.unitErrs.log("admin: unit state", err)
		if err == nil {
			res.Unit = &st
		}
	}
	return res, nil
}

// logOnce logs an error when it first appears or changes, so a status
// check every few seconds doesn't repeat it forever.
type logOnce struct {
	mu   sync.Mutex
	last string
}

func (l *logOnce) log(name string, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if msg != l.last && msg != "" {
		log.Printf("%s: %s", name, msg)
	}
	l.last = msg
}

// protect puts auth in front of h, except for /healthz: mc-agent's health
// check has no token. /healthz runs GET /api/deploy through h without auth,
// so it checks the app itself, and answers only ok or unavailable.
// An unconfigured admin is healthy: it's up, and refusing everyone.
func protect(h http.Handler, auth func(http.Handler) http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		probe := &statusOnly{code: http.StatusOK}
		req, err := http.NewRequestWithContext(r.Context(), "GET", "/api/deploy", nil)
		if err == nil {
			h.ServeHTTP(probe, req)
		}
		if err != nil || probe.code != http.StatusOK {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})
	mux.Handle("/", auth(h))
	return mux
}

// statusOnly is a ResponseWriter that keeps the status code and drops
// the body, so /healthz never passes on what the API returns.
type statusOnly struct {
	code   int
	header http.Header
}

func (s *statusOnly) Header() http.Header {
	if s.header == nil {
		s.header = http.Header{}
	}
	return s.header
}
func (s *statusOnly) Write(b []byte) (int, error) { return len(b), nil }
func (s *statusOnly) WriteHeader(code int)        { s.code = code }

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
	issuer, ok := teamIssuer(os.Getenv("ACCESS_TEAM_DOMAIN"))
	aud := strings.TrimSpace(os.Getenv("ACCESS_AUD"))
	if !ok {
		missing = append(missing, "ACCESS_TEAM_DOMAIN")
	}
	if aud == "" {
		missing = append(missing, "ACCESS_AUD")
	}
	if len(missing) > 0 {
		log.Printf("admin: not configured: set %s in ~/.config/minecraft/secrets.env (see deploy/secrets.example.env), then restart admin; refusing every request until then", strings.Join(missing, " and "))
		return func(http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Details are in the log, not for whoever is asking.
				http.Error(w, "admin isn't configured yet", http.StatusServiceUnavailable)
			})
		}
	}
	v := &access.Verifier{Issuer: issuer, AUD: aud}
	return v.Middleware
}

// teamIssuer turns ACCESS_TEAM_DOMAIN into the issuer URL tokens carry. It
// accepts the domain pasted as a URL too (https://myteam.cloudflareaccess.com/),
// and reports false if what's left isn't a plain host name.
func teamIssuer(domain string) (string, bool) {
	host := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(domain), "https://"), "/")
	if host == "" || strings.ContainsAny(host, "/:@ ") {
		return "", false
	}
	return "https://" + host, true
}

// envOr returns the first of the named environment variables that's set,
// or def. mc-backup.sh lets an MC_ name win over the plain one, and so
// does this.
func envOr(def string, names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return def
}

// flagSet reports whether the flag was given on the command line.
func flagSet(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("admin: %v", err)
	}
	dir := flag.String("dir", "", "folder with the page's static files (required)")
	sharedDir := flag.String("shared", "", "folder with the files all sites share, served at /shared/ (required)")
	appsDir := flag.String("apps", filepath.Join(home, "apps"), "folder mc-agent installs releases into")
	serverDir := flag.String("server", filepath.Join(home, "minecraft_server"), "Minecraft server folder, holding plugins/, whitelist.json and ops.json")
	addr := flag.String("addr", "127.0.0.1:8081", "host:port to listen on")
	paper := flag.String("paper", "127.0.0.1:25566", "Paper's address (answers only while awake)")
	lazymc := flag.String("lazymc", "127.0.0.1:25565", "LazyMC's address (answers while Paper sleeps)")
	timeout := flag.Duration("timeout", 2*time.Second, "how long to wait for each ping")
	rconAddr := flag.String("rcon", "127.0.0.1:25575", "Paper's RCON address (answers only while awake)")
	logPath := flag.String("logs", "", "Paper's log file (default: logs/latest.log in -server)")
	backupDir := flag.String("backups", envOr(filepath.Join(home, "backups", "minecraft"), "MC_BACKUP_DIR", "BACKUP_DIR"), "folder mc-backup.sh writes world backups to")
	keepDays := flag.Int("backup-keep-days", backups.DefaultKeepDays, "days the nightly cleanup keeps a backup that isn't flagged (default: BACKUP_KEEP_DAYS, else 10)")
	devUser := flag.String("dev-user", "", "skip the Access token check and act as this email (local development only)")
	flag.Parse()
	if !flagSet("backup-keep-days") {
		if v := envOr("", "MC_BACKUP_KEEP_DAYS", "BACKUP_KEEP_DAYS"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				log.Fatalf("admin: BACKUP_KEEP_DAYS must be a whole number of at least 1, got %q", v)
			}
			*keepDays = n
		}
	}
	if *keepDays < 1 {
		log.Fatalf("admin: -backup-keep-days must be at least 1")
	}
	if *logPath == "" {
		*logPath = filepath.Join(*serverDir, "logs", "latest.log")
	}

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

	a := &app{
		dir: *dir, sharedDir: *sharedDir, appsDir: *appsDir, serverDir: *serverDir,
		checker: &mcstatus.Checker{PaperAddr: *paper, LazymcAddr: *lazymc, Timeout: *timeout, Ping: ping.Status},
		logPath: *logPath,
		backups: &backups.Store{Dir: *backupDir, KeepDays: *keepDays},
		wake: func(ctx context.Context) error {
			return ping.Wake(ctx, *lazymc, wakeName)
		},
	}
	if ctl, err := control.NewSystemd(control.Unit, nil); err != nil {
		log.Fatalf("admin: %v", err)
	} else {
		a.control = ctl
	}
	if ctl, err := control.NewSystemd(control.BackupUnit, nil); err != nil {
		log.Fatalf("admin: %v", err)
	} else {
		a.backupUnit = ctl
	}
	// Read the password once and drop it from the environment, so
	// systemctl and anything else admin runs never inherit it.
	if pw := os.Getenv("RCON_PASSWORD"); pw != "" {
		os.Unsetenv("RCON_PASSWORD")
		a.rcon = func(ctx context.Context, cmd string) (string, error) {
			return rcon.Run(ctx, *rconAddr, pw, cmd)
		}
	} else {
		log.Printf("admin: RCON_PASSWORD isn't set, so the console is off; see deploy/secrets.example.env")
	}

	log.Printf("serving %s and the API on http://%s", *dir, *addr)
	log.Fatal(site.Server(*addr, protect(newHandler(a), auth)).ListenAndServe())
}
