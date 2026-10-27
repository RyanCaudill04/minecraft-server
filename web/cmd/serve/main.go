// Command serve serves a folder of static files over HTTP. It backs the
// placeholder sites behind the Cloudflare tunnel.
//
// Usage: serve -dir folder [-addr host:port]
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
)

func main() {
	dir := flag.String("dir", "", "folder to serve (required)")
	addr := flag.String("addr", "127.0.0.1:8080", "host:port to listen on")
	flag.Parse()

	// Require -dir: defaulting to the current folder could expose whatever
	// directory the program happens to start in.
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "serve: set -dir (e.g. -dir sites/play)")
		os.Exit(2)
	}

	info, err := os.Stat(*dir)
	if err != nil {
		log.Fatalf("serve: cannot use -dir: %v", err)
	}
	if !info.IsDir() {
		log.Fatalf("serve: %s is not a directory", *dir)
	}

	files := http.FileServer(http.Dir(*dir))
	log.Printf("serving %s on http://%s", *dir, *addr)
	log.Fatal(http.ListenAndServe(*addr, files))
}
