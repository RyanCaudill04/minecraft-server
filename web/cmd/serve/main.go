// Command serve serves a folder of static files over HTTP. It backs the
// placeholder sites behind the Cloudflare tunnel.
//
// Usage: serve -dir folder [-addr host:port]
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/ryancaudill04/minecraft-server/web/internal/site"
)

func main() {
	dir := flag.String("dir", "", "folder to serve (required)")
	addr := flag.String("addr", "127.0.0.1:8080", "host:port to listen on")
	flag.Parse()

	// Require -dir: defaulting to the current folder could expose whatever
	// directory the program happens to start in.
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "serve: set -dir (e.g. -dir sites/portfolio)")
		os.Exit(2)
	}

	info, err := os.Stat(*dir)
	if err != nil {
		log.Fatalf("serve: cannot use -dir: %v", err)
	}
	if !info.IsDir() {
		log.Fatalf("serve: %s is not a directory", *dir)
	}

	// The same file server as play and admin, so listings and dotfiles stay
	// hidden here too. These sites don't use the shared stylesheet.
	log.Printf("serving %s on http://%s", *dir, *addr)
	log.Fatal(site.Server(*addr, site.Static(*dir, "")).ListenAndServe())
}
