// Command play serves play.ryancaudill.dev: the static page in -dir plus a
// small JSON API reporting whether the Minecraft server is active, sleeping,
// starting or offline, and who is online.
//
// LazyMC answers status pings itself while Paper sleeps, so one ping can't
// tell the states apart. play pings Paper directly first, then LazyMC. Both
// are status pings, so checking never wakes the server.
//
// Usage: play -dir folder [-addr host:port] [-paper host:port] [-lazymc host:port]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/ping"
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

// checker pings the server and caches the result, so many visitors polling
// the page cause at most one round of pings per cache period.
type checker struct {
	paperAddr  string
	lazymcAddr string
	timeout    time.Duration
	maxAge     time.Duration

	mu   sync.Mutex
	last *Status
}

// status returns the cached result, refreshing it when it is older than
// maxAge. The lock is held during the pings, so concurrent requests wait for
// one refresh instead of each pinging the server.
func (c *checker) status(ctx context.Context) Status {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.last != nil && time.Since(c.last.Checked) < c.maxAge {
		return *c.last
	}
	s := c.check(ctx)
	c.last = &s
	return s
}

// check works out the server state: Paper answering means active; otherwise
// LazyMC answering means sleeping (or starting, going by its MOTD); otherwise
// offline.
func (c *checker) check(ctx context.Context) Status {
	s := Status{State: stateOffline, Players: []Player{}, Checked: time.Now().UTC()}

	if resp, err := c.ping(ctx, c.paperAddr); err == nil {
		s.State = stateActive
		s.Online = resp.Players.Online
		s.Max = resp.Players.Max
		s.Version = resp.Version.Name
		for _, p := range resp.Players.Sample {
			s.Players = append(s.Players, Player{Name: p.Name, ID: p.ID})
		}
		return s
	}

	if resp, err := c.ping(ctx, c.lazymcAddr); err == nil {
		s.State = stateSleeping
		if strings.Contains(strings.ToLower(resp.Description.Text), "starting") {
			s.State = stateStarting
		}
		s.Max = resp.Players.Max
		s.Version = resp.Version.Name
		return s
	}

	return s
}

// ping runs one status ping with its own timeout, so a dead Paper doesn't
// use up the time meant for LazyMC.
func (c *checker) ping(ctx context.Context, addr string) (*ping.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return ping.Status(ctx, addr)
}

func (c *checker) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	// Don't let one visitor closing the tab cancel the pings: the result is
	// cached and shared, and a cancelled ping would be cached as "offline".
	ctx := context.WithoutCancel(r.Context())
	if err := json.NewEncoder(w).Encode(c.status(ctx)); err != nil {
		log.Printf("play: writing status: %v", err)
	}
}

func main() {
	dir := flag.String("dir", "", "folder with the page's static files (required)")
	addr := flag.String("addr", "127.0.0.1:8082", "host:port to listen on")
	paper := flag.String("paper", "127.0.0.1:25566", "Paper's address (answers only while awake)")
	lazymc := flag.String("lazymc", "127.0.0.1:25565", "LazyMC's address (answers while Paper sleeps)")
	timeout := flag.Duration("timeout", 2*time.Second, "how long to wait for each ping")
	maxAge := flag.Duration("cache", 5*time.Second, "how long to reuse a status result")
	flag.Parse()

	if *dir == "" {
		fmt.Fprintln(os.Stderr, "play: set -dir (e.g. -dir sites/play)")
		os.Exit(2)
	}
	info, err := os.Stat(*dir)
	if err != nil {
		log.Fatalf("play: cannot use -dir: %v", err)
	}
	if !info.IsDir() {
		log.Fatalf("play: %s is not a directory", *dir)
	}

	c := &checker{paperAddr: *paper, lazymcAddr: *lazymc, timeout: *timeout, maxAge: *maxAge}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", c.handleStatus)
	mux.Handle("GET /", http.FileServer(http.Dir(*dir)))

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("serving %s and /api/status on http://%s", *dir, *addr)
	log.Fatal(srv.ListenAndServe())
}
