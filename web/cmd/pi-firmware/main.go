// Command pi-firmware changes the Raspberry Pi 5's clock, voltage,
// temperature-limit and fan settings for admin, as root. See package
// pifirmware for how a change is tried before it's kept.
//
// It is installed by hand, root-owned, at /usr/local/sbin/pi-firmware, and
// never by mc-agent, so code that runs as root never deploys itself from
// main. admin runs it through one sudoers rule (deploy/pi-firmware) that
// names each subcommand, and pi-firmware-boot.service runs "boot" at every
// boot and "clean-stop" at every clean shutdown. Nothing from a request
// reaches its arguments: "stage" and
// "governor" read their one JSON value from stdin, and it's checked again
// here, not trusted from admin.
//
//	stage         {"arm_freq": 2600, ...} on stdin: write tryboot.txt
//	discard       drop tryboot.txt (during a trial: go back on next reboot)
//	keep          copy a trial that passed into config.txt
//	reboot-trial  reboot once onto tryboot.txt
//	reboot        reboot normally
//	governor      {"governor": "performance"} on stdin: set it now and at boot
//	boot          at boot: the governor, probation, and checking a trial
//	clean-stop    at a clean shutdown: the boot ended cleanly
//	version       print the version
//
// It exits 2 when it refuses, with a message for the person on stderr,
// and 1 when something fails, except "boot", which reports errors but
// never fails, so its unit stays active and its clean-stop still runs at
// shutdown.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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
	lazymcAddr = "127.0.0.1:25565"
	// The trial's CPU check: every core hashes loadSize bytes, well
	// beyond the caches, for loadFor.
	loadFor    = 3 * time.Minute
	loadSize   = 256 << 20
	runTimeout = 30 * time.Second
)

// commands are the only programs pi-firmware runs, by full path.
var commands = map[string]string{
	"systemctl": "/usr/bin/systemctl",
	// get_throttled's sysfs file is gone from the 6.12 Pi kernels.
	"vcgencmd": "/usr/bin/vcgencmd",
}

func main() {
	if len(os.Args) != 2 {
		fail(2, "usage: pi-firmware stage|discard|keep|reboot-trial|reboot|governor|boot|clean-stop|version")
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

	h := &pifirmware.Host{Run: run, ServerUp: serverUp}
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
		// pi-firmware-boot.service sets the user whose minecraft.service
		// a trial boot keeps stopped.
		h.User = os.Getenv("PI_FIRMWARE_USER")
		h.Throttled = throttled
		h.Load = func(ctx context.Context) error { return pifirmware.CPUCheck(ctx, loadFor, loadSize) }
		if err := h.Boot(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "pi-firmware: boot: "+err.Error())
		}
		return
	case "clean-stop":
		err = h.CleanStop()
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

// serverUp reports whether LazyMC (minecraft.service) answers on the
// Minecraft port. It holds the port whenever the server is running, even
// while Paper sleeps.
func serverUp() bool {
	c, err := net.DialTimeout("tcp", lazymcAddr, 2*time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// throttled reads the firmware's get_throttled bits.
func throttled(ctx context.Context) (uint32, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, commands["vcgencmd"], "get_throttled").Output()
	if err != nil {
		return 0, err
	}
	return pifirmware.ParseThrottled(string(out))
}
