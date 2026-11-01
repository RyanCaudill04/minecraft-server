// Command mc-agent runs on the Raspberry Pi and keeps the web apps deployed.
// A systemd timer runs "mc-agent deploy" every few minutes: it installs the
// newest GitHub Release of this repo, health-checks it, and rolls back if
// it fails. Later the admin page will trigger the same actions.
//
// "mc-agent status" prints the active, failed and installed releases, the
// last deploy's result and the newest release on GitHub; "status -json"
// prints the same as JSON for scripts.
//
// Usage: mc-agent [flags] deploy|rollback|status [-json]|version
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/deploy"
)

// version is set at build time with -ldflags "-X main.version=<tag>".
var version = "dev"

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("mc-agent: %v", err)
	}
	repo := flag.String("repo", "RyanCaudill04/minecraft-server", "GitHub repo to deploy releases from")
	apps := flag.String("apps", filepath.Join(home, "apps"), "folder for installed releases")
	units := flag.String("units", filepath.Join(home, ".config", "systemd", "user"), "systemd user unit folder")
	api := flag.String("api", "https://api.github.com", "GitHub API base URL")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: mc-agent [flags] deploy|rollback|status [-json]|version")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}
	cmd, args := flag.Arg(0), flag.Args()[1:]
	statusFlags := flag.NewFlagSet("status", flag.ExitOnError)
	asJSON := statusFlags.Bool("json", false, "print the status as JSON")
	if cmd == "status" {
		statusFlags.Parse(args)
		args = statusFlags.Args()
	}
	if len(args) > 0 {
		flag.Usage()
		os.Exit(2)
	}

	d := &deploy.Deployer{
		Repo:          *repo,
		API:           *api,
		Apps:          *apps,
		UnitDir:       *units,
		HTTP:          &http.Client{Timeout: 2 * time.Minute},
		Systemctl:     systemctl,
		HealthTimeout: 20 * time.Second,
		Keep:          5,
		Log:           log.New(os.Stderr, "", 0), // journald adds timestamps
	}
	// Only commands that change things create the apps folder, so status
	// on a Pi without one doesn't leave an empty folder behind.
	if cmd == "deploy" || cmd == "rollback" {
		if err := os.MkdirAll(*apps, 0o755); err != nil {
			log.Fatalf("mc-agent: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	switch cmd {
	case "deploy":
		err = d.Run(ctx)
	case "rollback":
		err = d.Rollback(ctx)
	case "status":
		var s *deploy.Status
		if s, err = d.Status(ctx); err == nil {
			if *asJSON {
				err = printJSON(os.Stdout, s)
			} else {
				printStatus(os.Stdout, s)
			}
		}
	case "version":
		fmt.Println(version)
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatalf("mc-agent: %v", err)
	}
}

// systemctl runs "systemctl --user" with args and includes its output in
// any error.
func systemctl(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// statusJSON is what "status -json" prints: the deploy status plus the
// agent's own version.
type statusJSON struct {
	Agent string `json:"agent"`
	*deploy.Status
}

func printJSON(w io.Writer, s *deploy.Status) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(statusJSON{Agent: version, Status: s})
}

// printStatus prints s for a person, one fact per line.
func printStatus(w io.Writer, s *deploy.Status) {
	at := func(t time.Time) string { return t.Local().Format("2006-01-02 15:04 MST") }
	line := func(label, format string, a ...any) {
		fmt.Fprintf(w, "%-10s %s\n", label, fmt.Sprintf(format, a...))
	}

	line("agent", "%s", version)
	switch {
	case s.Active == "":
		line("active", "none")
	case s.AppliedAt.IsZero():
		line("active", "%s", s.Active)
	default:
		line("active", "%s, applied %s", s.Active, at(s.AppliedAt))
	}
	switch {
	case s.LatestError != "":
		line("latest", "unknown: %s", s.LatestError)
	case s.Behind():
		line("latest", "%s (not active yet)", s.Latest)
	case s.Latest != "":
		line("latest", "%s (up to date)", s.Latest)
	}
	if s.Failed != "" {
		line("failed", "%s (not retried until a newer release)", s.Failed)
	}
	if len(s.Installed) == 0 {
		line("installed", "none")
	} else {
		line("installed", "%s", strings.Join(s.Installed, " "))
	}
	if r := s.LastResult; r != nil {
		msg := r.Tag + " " + strings.ReplaceAll(r.Outcome, "_", " ")
		if r.RolledBackTo != "" {
			msg += " to " + r.RolledBackTo
		}
		msg += " at " + at(r.At)
		if r.Error != "" {
			msg += ": " + r.Error
		}
		line("last", "%s", msg)
	}
	for _, msg := range s.Warnings {
		line("warning", "%s", msg)
	}
}
