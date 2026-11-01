// Command mc-players prints the server's online player count and names using
// a status ping, so it does not wake a sleeping LazyMC proxy. It is a dev
// tool, not part of a release. The Paper boot test in CI (#104) uses it.
//
// Usage: mc-players [-addr host:port]   (defaults to $MC_ADDR)
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/ping"
)

func main() {
	addr := flag.String("addr", os.Getenv("MC_ADDR"), "server address as host:port")
	timeout := flag.Duration("timeout", 5*time.Second, "how long to wait for the server")
	flag.Parse()

	if *addr == "" {
		fmt.Fprintln(os.Stderr, "mc-players: set -addr or MC_ADDR (e.g. 192.168.1.10:25565)")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	resp, err := ping.Status(ctx, *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mc-players:", err)
		os.Exit(1)
	}

	fmt.Printf("%d/%d online\n", resp.Players.Online, resp.Players.Max)
	for _, p := range resp.Players.Sample {
		fmt.Println(" -", p.Name)
	}
	// Servers cap the sample (Paper defaults to 12), so it can be shorter
	// than the online count.
	if hidden := resp.Players.Online - len(resp.Players.Sample); hidden > 0 {
		fmt.Printf(" ...and %d more\n", hidden)
	}
}
