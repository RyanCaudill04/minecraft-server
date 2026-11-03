// Command mc-rcon runs one Minecraft server command over RCON and prints
// its output, for use over SSH now that Paper has no console.
//
// The password comes from RCON_PASSWORD, or else from the RCON_PASSWORD
// line in -secrets (~/.config/minecraft/secrets.env), never from a flag,
// which would show up in ps. RCON only answers while Paper is awake.
//
// Usage: mc-rcon [-addr host:port] [-secrets file] command [args...]
//
//	mc-rcon list
//	mc-rcon say Restarting in 5 minutes
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/rcon"
)

func main() {
	home, _ := os.UserHomeDir()
	addr := flag.String("addr", "127.0.0.1:25575", "Paper's RCON address")
	secrets := flag.String("secrets", filepath.Join(home, ".config", "minecraft", "secrets.env"), "file with an RCON_PASSWORD= line, used if RCON_PASSWORD isn't set")
	timeout := flag.Duration("timeout", 10*time.Second, "how long to wait for the server")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: mc-rcon [-addr host:port] [-secrets file] command [args...]")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}

	pw := os.Getenv("RCON_PASSWORD")
	if pw == "" {
		var err error
		if pw, err = readPassword(*secrets); err != nil {
			fmt.Fprintf(os.Stderr, "mc-rcon: %v\n", err)
			os.Exit(1)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	cmd := strings.TrimPrefix(strings.Join(flag.Args(), " "), "/")
	out, err := rcon.Run(ctx, *addr, pw, cmd)
	if errors.Is(err, rcon.ErrUnreachable) {
		fmt.Fprintln(os.Stderr, "mc-rcon: RCON isn't answering; Paper may be asleep (LazyMC wakes it when someone joins)")
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "mc-rcon: %v\n", err)
		os.Exit(1)
	}
	out = regexp.MustCompile("§.").ReplaceAllString(out, "")
	if out != "" {
		fmt.Println(strings.TrimRight(out, "\n"))
	}
}

// readPassword finds RCON_PASSWORD=... in an env file, the format systemd's
// EnvironmentFile reads. Surrounding quotes are dropped.
func readPassword(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("RCON_PASSWORD isn't set and %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok || strings.TrimSpace(k) != "RCON_PASSWORD" {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		if v != "" {
			return v, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("no RCON_PASSWORD in %s", path)
}
