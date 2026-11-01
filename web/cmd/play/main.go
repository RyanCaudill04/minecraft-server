// Command play serves play.ryancaudill.dev: the static page in -dir plus a
// small JSON API about the Minecraft server.
//
//	GET /api/status   active, sleeping, starting or offline, and who is online
//	GET /api/world    world facts from the saved world files
//	GET /api/players  every known player's saved stats
//	GET /api/time     the in-game clock and weather, run forward while active
//	GET /api/activity recent joins, leaves, deaths and advancements (see live.go)
//
// The status comes from status pings (see package mcstatus), so checking
// never wakes the server. World and player data come from files on disk, so
// they work while the server sleeps.
//
// Usage: play -dir folder -shared folder -server folder [-addr host:port] [-paper host:port] [-lazymc host:port]
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

	"github.com/ryancaudill04/minecraft-server/web/internal/mcstatus"
	"github.com/ryancaudill04/minecraft-server/web/internal/ping"
	"github.com/ryancaudill04/minecraft-server/web/internal/site"
	"github.com/ryancaudill04/minecraft-server/web/internal/world"
)

// newHandler routes the API and serves the static page from dir and the
// shared files from sharedDir. World and player data are read from
// serverDir.
func newHandler(dir, sharedDir, serverDir string, c *mcstatus.Checker) http.Handler {
	worldDir := filepath.Join(serverDir, "world")
	status := &site.Cache[mcstatus.Status]{MaxAge: 5 * time.Second, Load: c.Check}
	worldInfo := &site.Cache[*world.Info]{MaxAge: 30 * time.Second, Load: func(context.Context) (*world.Info, error) {
		return world.ReadInfo(worldDir)
	}}
	players := &site.Cache[[]world.Player]{MaxAge: 30 * time.Second, Load: func(context.Context) ([]world.Player, error) {
		return world.ReadPlayers(serverDir, worldDir)
	}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", site.JSON("play: status", status))
	mux.HandleFunc("GET /api/world", site.JSON("play: world", worldInfo))
	mux.HandleFunc("GET /api/players", site.JSON("play: players", players))
	addLive(mux, serverDir, status)
	mux.Handle("GET /", site.Static(dir, sharedDir))
	return mux
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
	c := &mcstatus.Checker{PaperAddr: *paper, LazymcAddr: *lazymc, Timeout: *timeout, Ping: ping.Status}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           newHandler(*dir, *sharedDir, *serverDir, c),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("serving %s and the API on http://%s", *dir, *addr)
	log.Fatal(srv.ListenAndServe())
}
