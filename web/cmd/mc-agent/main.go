// Command mc-agent runs on the Raspberry Pi and keeps the web apps deployed.
// A systemd timer runs "mc-agent deploy" every few minutes: it installs the
// newest GitHub Release of this repo, health-checks it, and rolls back if
// it fails. Later the admin page will trigger the same actions.
//
// "rollback" goes back to the release deployed before the current one and
// pins it, so the timer doesn't deploy the newer release again until
// "unpin". The agent deploying a release runs that release's own agent with
// "selfcheck", to make sure it can run the next deploy.
//
// Usage: mc-agent [flags] deploy|rollback|unpin|status|selfcheck|version
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
		fmt.Fprintln(os.Stderr, "usage: mc-agent [flags] deploy|rollback|unpin|status|selfcheck|version")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
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
		AgentTimer:    "mc-agent.timer",
		HealthTimeout: 20 * time.Second,
		Keep:          5,
		Log:           log.New(os.Stderr, "", 0), // journald adds timestamps
	}
	// The timer runs the agent through current, so run the new release's
	// agent the same way, with the same flags.
	d.CheckAgent = func(ctx context.Context) error {
		agent := filepath.Join(*apps, "current", "bin", "mc-agent")
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		out, err := exec.CommandContext(ctx, agent,
			"-repo", *repo, "-apps", *apps, "-units", *units, "-api", *api, "selfcheck").CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s selfcheck: %w: %s", agent, err, strings.TrimSpace(string(out)))
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	switch flag.Arg(0) {
	case "deploy":
		err = d.Run(ctx)
	case "rollback":
		err = d.Rollback(ctx)
	case "unpin":
		err = d.Unpin()
	case "selfcheck":
		err = d.SelfCheck(ctx, version)
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
