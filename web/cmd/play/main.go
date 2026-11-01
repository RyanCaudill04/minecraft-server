// Command play serves play.ryancaudill.dev: the static page in -dir plus a
// small JSON API about the Minecraft server.
//
//	GET /api/status   active, sleeping, starting or offline, and who is online
//	GET /api/world    world facts from the saved world files
//	GET /api/players  saved stats of the players who are public
//
// The status comes from status pings (see package mcstatus), so checking
// never wakes the server. World and player data come from files on disk, so
// they work while the server sleeps.
//
// Players are shown by name only if the server list shows them by name
// ("Allow Server Listings" on); see package listing and the Privacy section
// of the README. Hidden players count in totals and nowhere else.
//
// Usage: play -dir folder -shared folder -server folder [-addr host:port] [-paper host:port] [-lazymc host:port]
//
// With $STATE_DIRECTORY set (systemd's StateDirectory=), the listed players
// are kept in listed.json there, so a restart doesn't forget them.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/listing"
	"github.com/ryancaudill04/minecraft-server/web/internal/mcstatus"
	"github.com/ryancaudill04/minecraft-server/web/internal/ping"
	"github.com/ryancaudill04/minecraft-server/web/internal/site"
	"github.com/ryancaudill04/minecraft-server/web/internal/world"
)

// watchEvery is how often play pings the server to learn who is listed,
// whether or not anyone has the page open. Status pings never wake it.
const watchEvery = 15 * time.Second

// newHandler routes the API and serves the static page from dir and the
// shared files from sharedDir. World and player data are read from
// serverDir. reg decides which players are public.
func newHandler(dir, sharedDir, serverDir string, c *mcstatus.Checker, reg *listing.Registry) http.Handler {
	worldDir := filepath.Join(serverDir, "world")
	status := &site.Cache[mcstatus.Status]{MaxAge: 5 * time.Second, Load: func(ctx context.Context) (mcstatus.Status, error) {
		return check(ctx, c, reg)
	}}
	worldInfo := &site.Cache[*world.Info]{MaxAge: 30 * time.Second, Load: func(context.Context) (*world.Info, error) {
		return world.ReadInfo(worldDir)
	}}
	players := &site.Cache[[]world.Player]{MaxAge: 30 * time.Second, Load: func(context.Context) ([]world.Player, error) {
		return world.ReadPlayers(serverDir, worldDir)
	}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", site.JSON("play: status", status))
	mux.HandleFunc("GET /api/world", site.JSON("play: world", worldInfo))
	mux.HandleFunc("GET /api/players", func(w http.ResponseWriter, r *http.Request) {
		all, err := players.Get(context.WithoutCancel(r.Context()))
		if err != nil {
			log.Printf("play: players: %v", err)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		// Filtered per request, not cached: reg changes as pings come in.
		site.WriteJSON(w, "play: players", publicPlayers(all, reg))
	})
	mux.Handle("GET /", site.Static(dir, sharedDir))
	return mux
}

// check pings the server and records who Paper lists by name.
func check(ctx context.Context, c *mcstatus.Checker, reg *listing.Registry) (mcstatus.Status, error) {
	s, err := c.Check(ctx)
	if err == nil && s.State == mcstatus.StateActive {
		if err := reg.Observe(s.Players, s.Checked); err != nil {
			log.Printf("play: %v", err)
		}
	}
	return s, err
}

// publicPlayers keeps the players reg allows to be shown, in order.
func publicPlayers(all []world.Player, reg *listing.Registry) []world.Player {
	out := make([]world.Player, 0, len(all))
	for _, p := range all {
		if reg.Public(p.ID, p.LastSeen) {
			out = append(out, p)
		}
	}
	return out
}

// watch pings every interval until ctx ends, so a listed session is seen
// even while nobody has the page open.
func watch(ctx context.Context, c *mcstatus.Checker, reg *listing.Registry, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		check(ctx, c, reg)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func main() {
	dir := flag.String("dir", "", "folder with the page's static files (required)")
	sharedDir := flag.String("shared", "", "folder with the files all sites share, served at /shared/ (required)")
	serverDir := flag.String("server", "", "Minecraft server folder, holding usercache.json and world/ (required)")
	addr := flag.String("addr", "127.0.0.1:8082", "host:port to listen on")
	paper := flag.String("paper", "127.0.0.1:25566", "Paper's address (answers only while awake)")
	lazymc := flag.String("lazymc", "127.0.0.1:25565", "LazyMC's address (answers while Paper sleeps)")
	timeout := flag.Duration("timeout", 2*time.Second, "how long to wait for each ping")
	flag.Parse()

	if *dir == "" || *sharedDir == "" || *serverDir == "" {
		fmt.Fprintln(os.Stderr, "play: set -dir, -shared and -server (e.g. -dir sites/play -shared sites/shared -server ~/minecraft_server)")
		os.Exit(2)
	}
	for _, d := range []string{*dir, *sharedDir, *serverDir} {
		info, err := os.Stat(d)
		if err != nil {
			log.Fatalf("play: %v", err)
		}
		if !info.IsDir() {
			log.Fatalf("play: %s is not a directory", d)
		}
	}
	var statePath string
	if d := os.Getenv("STATE_DIRECTORY"); d != "" {
		statePath = filepath.Join(d, "listed.json")
	}
	reg, err := listing.Open(statePath, listing.DefaultGrace)
	if err != nil {
		log.Printf("play: %v (starting with nobody listed)", err)
	}
	c := &mcstatus.Checker{PaperAddr: *paper, LazymcAddr: *lazymc, Timeout: *timeout, Ping: ping.Status}
	go watch(context.Background(), c, reg, watchEvery)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           newHandler(*dir, *sharedDir, *serverDir, c, reg),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("serving %s and the API on http://%s", *dir, *addr)
	log.Fatal(srv.ListenAndServe())
}
