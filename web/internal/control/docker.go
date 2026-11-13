package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// Container is the Minecraft server's container in deploy/containers'
// compose.yaml. It runs LazyMC, which starts Paper on demand, like
// minecraft.service did.
const Container = "mc-server"

// DockerSocket is where the Docker Engine API listens by default.
const DockerSocket = "/var/run/docker.sock"

// validContainer is Docker's own rule for container names.
var validContainer = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// Docker controls one container through the Docker Engine API, the way
// Systemd controls a unit, and reports its state in systemd's terms so the
// admin page reads both the same way.
//
// A stop can take minutes while Paper saves, and the API call blocks until
// the container has exited. So Do waits only briefly for the call: a quick
// error (such as a missing container) is returned, and anything slower
// carries on in the background while State reports it as activating or
// deactivating, like systemctl --no-block.
//
// Only the container named when it was made is ever touched, and the path
// is built from fixed parts, so nothing from a request reaches the API.
type Docker struct {
	container string
	base      string // "http://docker", or a test server's URL
	client    *http.Client

	timeout  time.Duration // for each state read
	stopWait time.Duration // Docker's grace period before it kills the container
	wait     time.Duration // how long Do waits for the action before returning
	log      func(format string, args ...any)

	mu      sync.Mutex
	pending Action // the action running in the background, or ""
}

// NewDocker returns a controller for container through the Engine API on
// the unix socket at socket.
func NewDocker(container, socket string) (*Docker, error) {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return newDocker(container, "http://docker", &http.Client{Transport: tr})
}

func newDocker(container, base string, client *http.Client) (*Docker, error) {
	if !validContainer.MatchString(container) {
		return nil, fmt.Errorf("control: %q isn't a container name", container)
	}
	return &Docker{
		container: container, base: base, client: client,
		timeout: 15 * time.Second,
		// The same grace period compose.yaml gives mc-server, and what
		// minecraft.service gave it: longer than LazyMC's stop_timeout.
		stopWait: 240 * time.Second,
		wait:     2 * time.Second,
		log:      log.Printf,
	}, nil
}

// Do runs the action. It returns ErrBusy if another action is running or
// the container is between states.
func (d *Docker) Do(ctx context.Context, a Action) error {
	if _, ok := ParseAction(string(a)); !ok {
		return fmt.Errorf("control: unknown action %q", a)
	}
	d.mu.Lock()
	if d.pending != "" {
		d.mu.Unlock()
		return ErrBusy
	}
	d.pending = a
	d.mu.Unlock()

	// The container's own state, not the pending action just recorded.
	c, err := d.inspect(ctx)
	if err == nil && dockerState(c.State.Status, c.State.ExitCode, "").Busy() {
		err = ErrBusy
	}
	if err != nil {
		d.finish()
		return err
	}

	done := make(chan error, 1)
	go func() {
		defer d.finish()
		// Not the request's context: the action must outlive the request.
		ctx, cancel := context.WithTimeout(context.Background(), d.stopWait+time.Minute)
		defer cancel()
		err := d.post(ctx, a)
		if err != nil {
			d.log("control: %s %s: %v", a, d.container, err)
		}
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(d.wait):
		return nil // still going; State shows it
	case <-ctx.Done():
		return nil
	}
}

func (d *Docker) finish() {
	d.mu.Lock()
	d.pending = ""
	d.mu.Unlock()
}

// post asks Docker to start, stop or restart the container. 304 means it
// already was in that state, which is fine.
func (d *Docker) post(ctx context.Context, a Action) error {
	u := d.base + "/containers/" + url.PathEscape(d.container) + "/" + string(a)
	if a == Stop || a == Restart {
		u += "?t=" + strconv.Itoa(int(d.stopWait/time.Second))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("control: docker %s %s: %w", a, d.container, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotModified:
		return nil
	}
	return fmt.Errorf("control: docker %s %s: %s", a, d.container, apiError(resp))
}

// inspected is the part of GET /containers/{name}/json that State reads.
type inspected struct {
	State struct {
		Status   string `json:"Status"` // created, running, paused, restarting, removing, exited or dead
		ExitCode int    `json:"ExitCode"`
	} `json:"State"`
}

// State returns the container's state, in systemd's terms. While an action
// runs in the background, that's what it reports.
func (d *Docker) State(ctx context.Context) (State, error) {
	d.mu.Lock()
	pending := d.pending
	d.mu.Unlock()
	c, err := d.inspect(ctx)
	if err != nil {
		return State{}, err
	}
	return dockerState(c.State.Status, c.State.ExitCode, pending), nil
}

// inspect reads the container from GET /containers/{name}/json.
func (d *Docker) inspect(ctx context.Context) (*inspected, error) {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	u := d.base + "/containers/" + url.PathEscape(d.container) + "/json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("control: reading %s from docker: %w", d.container, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("control: reading %s from docker: %s", d.container, apiError(resp))
	}
	var c inspected
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&c); err != nil {
		return nil, fmt.Errorf("control: reading %s from docker: %w", d.container, err)
	}
	return &c, nil
}

// dockerState maps a container's status, and the action control is
// running on it, to systemd's ActiveState and SubState.
func dockerState(status string, exitCode int, pending Action) State {
	switch pending {
	case Start:
		if status != "running" {
			return State{Active: "activating", Sub: "start"}
		}
	case Stop, Restart:
		return State{Active: "deactivating", Sub: "stop-sigterm"}
	}
	switch status {
	case "running":
		return State{Active: "active", Sub: "running"}
	case "paused":
		return State{Active: "active", Sub: "paused"}
	case "restarting":
		return State{Active: "activating", Sub: "auto-restart"}
	case "removing":
		return State{Active: "deactivating", Sub: "removing"}
	case "created":
		return State{Active: "inactive", Sub: "dead"}
	case "exited":
		// 143 is 128+SIGTERM: LazyMC exiting because it was stopped.
		if exitCode == 0 || exitCode == 143 {
			return State{Active: "inactive", Sub: "dead"}
		}
		return State{Active: "failed", Sub: "exit-code"}
	case "dead":
		return State{Active: "failed", Sub: "failed"}
	}
	return State{Active: "failed", Sub: status}
}

// apiError reads the Engine API's {"message": "..."} error body.
func apiError(resp *http.Response) error {
	var body struct {
		Message string `json:"message"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body) == nil && body.Message != "" {
		return fmt.Errorf("%s: %s", resp.Status, body.Message)
	}
	return errors.New(resp.Status)
}
