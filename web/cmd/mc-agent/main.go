// Command mc-agent runs on the Raspberry Pi and keeps the web apps deployed.
// A systemd timer runs "mc-agent deploy" every few minutes: it installs the
// newest GitHub Release of this repo, health-checks it, and rolls back if
// it fails. Later the admin page will trigger the same actions.
//
// The repo to deploy from is -repo, else $REPO (host.env, see
// deploy/host.example.env), else the repo whose CI built this binary. A
// build with none of these refuses to deploy rather than guess.
//
// Usage: mc-agent [flags] deploy|rollback|status|version
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/deploy"
)

// version is set at build time with -ldflags "-X main.version=<tag>".
var version = "dev"

// builtRepo is the GitHub repo (owner/name) that built this binary, set
// at build time with -ldflags "-X main.builtRepo=<owner/name>". Empty in
// local builds.
var builtRepo = ""

// validRepo matches a GitHub owner/name. The name may start with one dot
// (.github) but can't be . or .., which would change the API path.
var validRepo = regexp.MustCompile(`^[A-Za-z0-9-]+/\.?[A-Za-z0-9_-][A-Za-z0-9_.-]*$`)

// defaultRepo is $REPO if set, else builtRepo.
func defaultRepo() string {
	if r := os.Getenv("REPO"); r != "" {
		return r
	}
	return builtRepo
}

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("mc-agent: %v", err)
	}
	repo := flag.String("repo", defaultRepo(), "GitHub repo (owner/name) to deploy releases from; defaults to $REPO, then the repo that built this binary")
	apps := flag.String("apps", filepath.Join(home, "apps"), "folder for installed releases")
	units := flag.String("units", filepath.Join(home, ".config", "systemd", "user"), "systemd user unit folder")
	api := flag.String("api", "https://api.github.com", "GitHub API base URL")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: mc-agent [flags] deploy|rollback|status|version")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	if flag.Arg(0) == "deploy" {
		if *repo == "" {
			log.Fatal("mc-agent: no repo to deploy from: set REPO=owner/name in ~/.config/minecraft/host.env (see deploy/host.example.env) or pass -repo")
		}
		if !validRepo.MatchString(*repo) {
			log.Fatalf("mc-agent: repo %q is not owner/name", *repo)
		}
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
	if err := os.MkdirAll(*apps, 0o755); err != nil {
		log.Fatalf("mc-agent: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	switch flag.Arg(0) {
	case "deploy":
		err = d.Run(ctx)
	case "rollback":
		err = d.Rollback(ctx)
	case "status":
		var current string
		current, err = d.Current()
		if err == nil {
			fmt.Printf("agent %s, active release %q\n", version, current)
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
