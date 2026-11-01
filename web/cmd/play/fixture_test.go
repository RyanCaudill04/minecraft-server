package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ryancaudill04/minecraft-server/web/internal/ping"
	"github.com/ryancaudill04/minecraft-server/web/internal/world/worldtest"
)

// TestFixtureServer serves the real page against the synthetic world for
// the Playwright tests in tests/play/e2e. It's skipped unless
// PLAY_FIXTURE_ADDR is set, and then serves until the process is killed:
//
//	PLAY_FIXTURE_ADDR=127.0.0.1:19150 go test ./cmd/play -run '^TestFixtureServer$' -count=1 -timeout 0
//
// Each server state gets its own port, counting up from PLAY_FIXTURE_ADDR:
// active (Alice online), sleeping, starting, then offline.
func TestFixtureServer(t *testing.T) {
	addr := os.Getenv("PLAY_FIXTURE_ADDR")
	if addr == "" {
		t.Skip("set PLAY_FIXTURE_ADDR to serve the e2e fixture")
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}

	// go test runs in the package folder, web/cmd/play.
	const dir, shared = "../../../sites/play", "../../../sites/shared"
	server := worldtest.Build(t)
	states := []struct {
		name      string
		responses map[string]*ping.Response
	}{
		{"active", map[string]*ping.Response{paperAddr: {
			Version: ping.Version{Name: "Paper 26.2"},
			Players: ping.Players{Online: 2, Max: 20, Sample: []ping.Player{
				{Name: "Alice", ID: worldtest.AliceID},
				{Name: "Anonymous Player", ID: "00000000-0000-0000-0000-000000000000"},
			}},
		}}},
		{"sleeping", map[string]*ping.Response{lazymcAddr: {
			Players:     ping.Players{Max: 20},
			Description: ping.Description{Text: "Server is sleeping"},
		}}},
		{"starting", map[string]*ping.Response{lazymcAddr: {
			Players:     ping.Players{Max: 20},
			Description: ping.Description{Text: "Server is Starting..."},
		}}},
		{"offline", nil},
	}

	errs := make(chan error, len(states))
	for i, s := range states {
		a := net.JoinHostPort(host, strconv.Itoa(port+i))
		ln, err := net.Listen("tcp", a)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Printf("fixture: %s on http://%s\n", s.name, a)
		go func() {
			errs <- http.Serve(ln, newHandler(dir, shared, server, filepath.Join(server, "plugins", "squaremap", "web"), fakeChecker(s.responses, nil)))
		}()
	}
	if err := <-errs; err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatal(err)
	}
}
