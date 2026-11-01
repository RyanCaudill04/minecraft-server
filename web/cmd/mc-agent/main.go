// Command mc-agent runs on the Raspberry Pi and keeps the web apps deployed.
// A systemd timer runs "mc-agent deploy" every few minutes: for each track
// (play, admin, sites and the agent itself) it installs the track's newest
// GitHub Release, health-checks it, and rolls that track back if it fails.
// A Pi still on the single release (r<N>) deploys that, and moves to one
// folder per track once every track has a release.
//
// "rollback <track>" goes back to the track's release deployed before the
// current one and pins it, so the timer doesn't deploy the newer release
// again until "unpin <track>". On the single release, leave out the track.
// "migrate" retries a move to tracks that failed with the same releases.
// The agent deploying a release runs that release's own agent with
// "selfcheck", to make sure it can run the next deploy.
//
// Usage: mc-agent [flags] deploy|rollback [track]|unpin [track]|migrate|status|selfcheck|version
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
		fmt.Fprintln(os.Stderr, "usage: mc-agent [flags] deploy|rollback [track]|unpin [track]|migrate|status|selfcheck|version")
		flag.PrintDefaults()
	}
	flag.Parse()
	cmd, track := flag.Arg(0), flag.Arg(1)
	takesTrack := cmd == "rollback" || cmd == "unpin"
	if flag.NArg() < 1 || flag.NArg() > 2 || flag.NArg() == 2 && !takesTrack {
		flag.Usage()
		os.Exit(2)
	}

	a := &deploy.Agent{
		Repo:    *repo,
		API:     *api,
		Apps:    *apps,
		UnitDir: *units,
		// Deployed in this order, the agent last whatever its place here.
		Tracks:        []deploy.Track{{Name: "play"}, {Name: "admin"}, {Name: "sites"}, {Name: "agent"}},
		AgentTrack:    "agent",
		AgentTimer:    "mc-agent.timer",
		HTTP:          &http.Client{Timeout: 2 * time.Minute},
		Systemctl:     systemctl,
		HealthTimeout: 20 * time.Second,
		Keep:          5,
		Log:           log.New(os.Stderr, "", 0), // journald adds timestamps
	}
	// The timer runs the agent through its release's current link, so run
	// the new release's agent the same way, with the same flags.
	a.CheckAgent = func(ctx context.Context, agent string) error {
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

	switch cmd {
	case "deploy":
		err = a.Run(ctx)
	case "migrate":
		err = a.Migrate(ctx)
	case "rollback":
		err = a.Rollback(ctx, track)
	case "unpin":
		err = a.Unpin(track)
	case "selfcheck":
		err = a.SelfCheck(ctx, version)
	case "status":
		var s *deploy.Status
		s, err = a.Status()
		if err == nil {
			fmt.Printf("agent %s, layout %s\n", version, s.Layout)
			for _, t := range s.Tracks {
				name := t.Track
				if name == "" {
					name = "single release"
				}
				fmt.Printf("  %s: active %q", name, t.Active)
				for _, f := range [][2]string{{"failed", t.Failed}, {"pinned", t.Pinned}, {"activating", t.Activating}} {
					if f[1] != "" {
						fmt.Printf(", %s %s", f[0], f[1])
					}
				}
				fmt.Println()
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
