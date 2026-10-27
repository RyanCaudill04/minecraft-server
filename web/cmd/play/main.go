// Command play serves play.ryancaudill.dev: the static page in -dir plus a
// small JSON API about the Minecraft server.
//
//	GET /api/status   active, sleeping, starting or offline, and who is online
//	GET /api/world    world facts from the saved world files
//	GET /api/players  every known player's saved stats
//
// LazyMC answers status pings itself while Paper sleeps, so one ping can't
// tell the states apart. play pings Paper directly first, then LazyMC. Both
// are status pings, so checking never wakes the server. World and player
// data come from files on disk, so they work while the server sleeps.
//
// Usage: play -dir folder -server folder [-addr host:port] [-paper host:port] [-lazymc host:port]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/ping"
	"github.com/ryancaudill04/minecraft-server/web/internal/world"
)

// Server states reported by /api/status.
const (
	stateActive   = "active"
	stateSleeping = "sleeping"
	stateStarting = "starting"
	stateOffline  = "offline"
)

// Status is the JSON body of GET /api/status.
type Status struct {
	State   string    `json:"state"`
	Online  int       `json:"online"`
	Max     int       `json:"max"`
	Players []Player  `json:"players"`
	Version string    `json:"version,omitempty"`
	Checked time.Time `json:"checked"`
}

// Player is one name from the server's player sample.
type Player struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

// cache holds the result of load for maxAge, so many visitors polling the
// page cause at most one load per period. The lock is held during load, so
// concurrent requests wait for one refresh instead of each doing the work.
type cache[T any] struct {
	maxAge time.Duration
	load   func(context.Context) (T, error)

	mu      sync.Mutex
	value   T
	err     error
	fetched time.Time
}

func (c *cache[T]) get(ctx context.Context) (T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.fetched.IsZero() || time.Since(c.fetched) >= c.maxAge {
		c.value, c.err = c.load(ctx)
		c.fetched = time.Now()
	}
	return c.value, c.err
}

// checker works out the server state from status pings.
type checker struct {
	paperAddr  string
	lazymcAddr string
	timeout    time.Duration
	// status pings one address; ping.Status, or a fake in tests.
	status func(ctx context.Context, addr string) (*ping.Response, error)
}

// check reports the server state: Paper answering means active; otherwise
// LazyMC answering means sleeping (or starting, going by its MOTD); otherwise
// offline. It never fails: an unreachable server is a valid answer.
func (c *checker) check(ctx context.Context) (Status, error) {
	s := Status{State: stateOffline, Players: []Player{}, Checked: time.Now().UTC()}

	if resp, err := c.ping(ctx, c.paperAddr); err == nil {
		s.State = stateActive
		s.Online = resp.Players.Online
		s.Max = resp.Players.Max
		s.Version = resp.Version.Name
		for _, p := range resp.Players.Sample {
			s.Players = append(s.Players, Player{Name: p.Name, ID: p.ID})
		}
		return s, nil
	}

	if resp, err := c.ping(ctx, c.lazymcAddr); err == nil {
		s.State = stateSleeping
		if strings.Contains(strings.ToLower(resp.Description.Text), "starting") {
			s.State = stateStarting
		}
		s.Max = resp.Players.Max
		s.Version = resp.Version.Name
		return s, nil
	}

	return s, nil
}

// ping runs one status ping with its own timeout, so a dead Paper doesn't
// use up the time meant for LazyMC.
func (c *checker) ping(ctx context.Context, addr string) (*ping.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return c.status(ctx, addr)
}

// serveJSON returns a handler that writes the cached value as JSON.
func serveJSON[T any](name string, c *cache[T]) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Don't let one visitor closing the tab cancel the load: the result
		// is cached and shared, and a cancelled ping would be cached as
		// "offline".
		v, err := c.get(context.WithoutCancel(r.Context()))
		if err != nil {
			log.Printf("play: %s: %v", name, err)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(v); err != nil {
			log.Printf("play: writing %s: %v", name, err)
		}
	}
}

// newHandler routes the API and serves the static page from dir. World and
// player data are read from serverDir.
func newHandler(dir, serverDir string, c *checker) http.Handler {
	worldDir := filepath.Join(serverDir, "world")
	status := &cache[Status]{maxAge: 5 * time.Second, load: c.check}
	worldInfo := &cache[*world.Info]{maxAge: 30 * time.Second, load: func(context.Context) (*world.Info, error) {
		return world.ReadInfo(worldDir)
	}}
	players := &cache[[]world.Player]{maxAge: 30 * time.Second, load: func(context.Context) ([]world.Player, error) {
		return world.ReadPlayers(serverDir, worldDir)
	}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", serveJSON("status", status))
	mux.HandleFunc("GET /api/world", serveJSON("world", worldInfo))
	mux.HandleFunc("GET /api/players", serveJSON("players", players))
	mux.Handle("GET /", http.FileServer(http.Dir(dir)))
	return mux
}

func main() {
	dir := flag.String("dir", "", "folder with the page's static files (required)")
	serverDir := flag.String("server", "", "Minecraft server folder, holding usercache.json and world/ (required)")
	addr := flag.String("addr", "127.0.0.1:8082", "host:port to listen on")
	paper := flag.String("paper", "127.0.0.1:25566", "Paper's address (answers only while awake)")
	lazymc := flag.String("lazymc", "127.0.0.1:25565", "LazyMC's address (answers while Paper sleeps)")
	timeout := flag.Duration("timeout", 2*time.Second, "how long to wait for each ping")
	flag.Parse()

	if *dir == "" || *serverDir == "" {
		fmt.Fprintln(os.Stderr, "play: set -dir and -server (e.g. -dir sites/play -server ~/minecraft_server)")
		os.Exit(2)
	}
	for _, d := range []string{*dir, *serverDir} {
		info, err := os.Stat(d)
		if err != nil {
			log.Fatalf("play: %v", err)
		}
		if !info.IsDir() {
			log.Fatalf("play: %s is not a directory", d)
		}
	}
	c := &checker{paperAddr: *paper, lazymcAddr: *lazymc, timeout: *timeout, status: ping.Status}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           newHandler(*dir, *serverDir, c),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("serving %s and the API on http://%s", *dir, *addr)
	log.Fatal(srv.ListenAndServe())
}
