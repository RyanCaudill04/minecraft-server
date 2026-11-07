// Command pi-firmware changes the Raspberry Pi 5's clock, voltage,
// temperature-limit and fan settings for admin, as root. See package
// pifirmware for how a change is tried before it's kept.
//
// It is installed by hand, root-owned, at /usr/local/sbin/pi-firmware, and
// never by mc-agent, so code that runs as root never deploys itself from
// main. admin runs it through one sudoers rule (deploy/pi-firmware) that
// names each subcommand, and pi-firmware-boot.service runs "boot" at every
// boot. Nothing from a request reaches its arguments: "stage" and
// "governor" read their one JSON value from stdin, and it's checked again
// here, not trusted from admin.
//
//	stage         {"arm_freq": 2600, ...} on stdin: write tryboot.txt
//	discard       drop tryboot.txt (during a trial: go back on next reboot)
//	keep          copy a trial that passed into config.txt
//	reboot-trial  reboot once onto tryboot.txt
//	reboot        reboot normally
//	governor      {"governor": "performance"} on stdin: set it now and at boot
//	boot          at boot: apply the governor, check or settle a trial
//	version       print the version
//
// It exits 2 when it refuses, with a message for the person on stderr,
// and 1 when something fails.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/pifirmware"
)

// version is set at build time with -ldflags "-X main.version=<tag>".
var version = "dev"

const (
	maxInput   = 4 << 10
	healthURL  = "http://127.0.0.1:8081/healthz" // admin
	healthWait = 5 * time.Minute
	loadFor    = time.Minute
	runTimeout = 30 * time.Second
)

// commands are the only programs Host may run, by full path.
var commands = map[string]string{"systemctl": "/usr/bin/systemctl"}

func main() {
	if len(os.Args) != 2 {
		fail(2, "usage: pi-firmware stage|discard|keep|reboot-trial|reboot|governor|boot|version")
	}
	cmd := os.Args[1]
	if cmd == "version" {
		fmt.Println(version)
		return
	}
	if os.Geteuid() != 0 {
		fail(1, "pi-firmware must run as root")
	}
	syscall.Umask(0o022)

	h := &pifirmware.Host{Run: run}
	ctx := context.Background()
	var err error
	switch cmd {
	case "stage":
		var s pifirmware.Settings
		if err = readInput(os.Stdin, &s); err == nil {
			err = h.Stage(s)
		}
	case "discard":
		err = h.Discard()
	case "keep":
		err = h.Keep()
	case "reboot-trial":
		err = h.RebootTrial(ctx)
	case "reboot":
		err = h.Reboot(ctx)
	case "governor":
		var g struct {
			Governor string `json:"governor"`
		}
		if err = readInput(os.Stdin, &g); err == nil {
			err = h.SetGovernor(g.Governor)
		}
	case "boot":
		h.Healthy, h.HealthWait = healthy, healthWait
		h.Load = func(ctx context.Context) error { return pifirmware.CPUCheck(ctx, loadFor) }
		err = h.Boot(ctx)
	default:
		fail(2, "pi-firmware: unknown command "+cmd)
	}
	var refused *pifirmware.RefusedError
	switch {
	case errors.As(err, &refused):
		fail(2, refused.Msg)
	case err != nil:
		fail(1, "pi-firmware: "+cmd+": "+err.Error())
	}
}

func fail(code int, msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(code)
}

// readInput decodes exactly one small JSON value from r into v, with no
// unknown fields.
func readInput(r io.Reader, v any) error {
	b, err := io.ReadAll(io.LimitReader(r, maxInput+1))
	if err != nil {
		return err
	}
	if len(b) > maxInput {
		return &pifirmware.RefusedError{Msg: "The request is too big."}
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &pifirmware.RefusedError{Msg: "Couldn't read the settings: they must be whole numbers."}
	}
	if dec.More() {
		return &pifirmware.RefusedError{Msg: "Couldn't read the settings."}
	}
	return nil
}

// run runs one of commands, without a shell.
func run(ctx context.Context, name string, args ...string) error {
	path, ok := commands[name]
	if !ok {
		return fmt.Errorf("%s isn't allowed", name)
	}
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

// healthy reports whether admin answers its health check.
func healthy(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", healthURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("admin answered %s", resp.Status)
	}
	return nil
}
