// Command mc-rcon runs console commands on the Minecraft server over RCON,
// in order, and prints each reply. The world backup uses it to pause
// saving while it copies the world.
//
// The password comes from $MC_RCON_PASSWORD, not a flag, so it doesn't
// show up in the process list.
//
// Usage: MC_RCON_PASSWORD=... mc-rcon [-addr host:port] command...
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/rcon"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:25575", "RCON address as host:port")
	timeout := flag.Duration("timeout", 60*time.Second, "how long all the commands may take")
	flag.Parse()

	password := os.Getenv("MC_RCON_PASSWORD")
	if password == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: MC_RCON_PASSWORD=... mc-rcon [-addr host:port] command...")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	c, err := rcon.Dial(ctx, *addr, password)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mc-rcon:", err)
		os.Exit(1)
	}
	defer c.Close()

	for _, cmd := range flag.Args() {
		reply, err := c.Command(cmd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mc-rcon: %s: %v\n", cmd, err)
			os.Exit(1)
		}
		if reply != "" {
			fmt.Println(reply)
		}
	}
}
