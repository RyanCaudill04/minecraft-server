// Package mcstatus works out whether the Minecraft server is active,
// sleeping, starting or offline.
//
// LazyMC answers status pings itself while Paper sleeps, so one ping can't
// tell the states apart. Checker pings Paper directly first, then LazyMC.
// Both are status pings, so checking never wakes the server.
package mcstatus

import (
	"context"
	"strings"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/ping"
)

// Server states reported in Status.State.
const (
	StateActive   = "active"
	StateSleeping = "sleeping"
	StateStarting = "starting"
	StateOffline  = "offline"
)

// Status is the server's state and who is online. Players lists only the
// players the server list shows by name; hidden players count in Online
// but never appear in Players.
type Status struct {
	State   string    `json:"state"`
	Online  int       `json:"online"`
	Max     int       `json:"max"`
	Players []Player  `json:"players"`
	Version string    `json:"version,omitempty"`
	Checked time.Time `json:"checked"`
}

// AnonymousID is the UUID Paper sends in place of players who turned off
// "Allow Server Listings".
const AnonymousID = "00000000-0000-0000-0000-000000000000"

// Player is one name from the server's player sample.
type Player struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

// Checker works out the server state from status pings.
type Checker struct {
	PaperAddr  string
	LazymcAddr string
	Timeout    time.Duration
	// Ping pings one address; ping.Status, or a fake in tests.
	Ping func(ctx context.Context, addr string) (*ping.Response, error)
}

// Check reports the server state: Paper answering means active; otherwise
// LazyMC answering means sleeping (or starting, going by its MOTD); otherwise
// offline. It never fails: an unreachable server is a valid answer.
func (c *Checker) Check(ctx context.Context) (Status, error) {
	s := Status{State: StateOffline, Players: []Player{}, Checked: time.Now().UTC()}

	if resp, err := c.ping(ctx, c.PaperAddr); err == nil {
		s.State = StateActive
		s.Online = resp.Players.Online
		s.Max = resp.Players.Max
		s.Version = resp.Version.Name
		for _, p := range resp.Players.Sample {
			if p.ID == AnonymousID || p.ID == "" {
				continue // hidden: counted in Online only
			}
			s.Players = append(s.Players, Player{Name: p.Name, ID: p.ID})
		}
		return s, nil
	}

	if resp, err := c.ping(ctx, c.LazymcAddr); err == nil {
		s.State = StateSleeping
		if strings.Contains(strings.ToLower(resp.Description.Text), "starting") {
			s.State = StateStarting
		}
		s.Max = resp.Players.Max
		s.Version = resp.Version.Name
		return s, nil
	}

	return s, nil
}

// ping runs one status ping with its own timeout, so a dead Paper doesn't
// use up the time meant for LazyMC.
func (c *Checker) ping(ctx context.Context, addr string) (*ping.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	return c.Ping(ctx, addr)
}
