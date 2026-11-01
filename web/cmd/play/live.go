package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/activity"
	"github.com/ryancaudill04/minecraft-server/web/internal/mcstatus"
	"github.com/ryancaudill04/minecraft-server/web/internal/site"
	"github.com/ryancaudill04/minecraft-server/web/internal/world"
)

// feedSize is how many events GET /api/activity returns.
const feedSize = 30

// anonymousID is the UUID Paper sends for players who hide themselves from
// the server list.
const anonymousID = "00000000-0000-0000-0000-000000000000"

// Activity is the body of GET /api/activity.
type Activity struct {
	Events []activity.Event `json:"events"` // newest first
}

// addLive adds the routes that follow the running game: the clock and
// weather run forward from the last save (GET /api/time), and the feed of
// joins, leaves, deaths and advancements from the server's log
// (GET /api/activity).
//
// The names of players seen in the server list are kept in visible.json
// under $STATE_DIRECTORY (set by systemd's StateDirectory=) when it's set,
// so the feed's history survives restarts.
func addLive(mux *http.ServeMux, serverDir string, status *site.Cache[mcstatus.Status]) {
	feed := &activity.Feed{Dir: filepath.Join(serverDir, "logs")}
	if dir, _, _ := strings.Cut(os.Getenv("STATE_DIRECTORY"), ":"); dir != "" {
		feed.StatePath = filepath.Join(dir, "visible.json")
	}

	// update reads new log lines and, while Paper is up, who it lists.
	update := func(ctx context.Context) mcstatus.Status {
		if err := feed.Update(); err != nil {
			log.Printf("play: activity: %v", err)
		}
		s, _ := status.Get(ctx)
		if s.State == mcstatus.StateActive {
			var names []string
			anonymous := false
			for _, p := range s.Players {
				if p.ID == anonymousID {
					anonymous = true
				} else {
					names = append(names, p.Name)
				}
			}
			if err := feed.Observe(names, anonymous); err != nil {
				log.Printf("play: activity: %v", err)
			}
		}
		return s
	}

	worldDir := filepath.Join(serverDir, "world")
	sky := &site.Cache[world.Now]{MaxAge: 5 * time.Second, Load: func(ctx context.Context) (world.Now, error) {
		s := update(ctx)
		saved, err := world.ReadSky(worldDir)
		if err != nil {
			return world.Now{}, err
		}
		since, running := feed.Running()
		if !running {
			since = time.Time{}
		}
		return saved.At(time.Now(), s.State == mcstatus.StateActive, since), nil
	}}
	feedCache := &site.Cache[Activity]{MaxAge: 5 * time.Second, Load: func(ctx context.Context) (Activity, error) {
		update(ctx)
		return Activity{Events: feed.Events(feedSize)}, nil
	}}

	mux.HandleFunc("GET /api/time", site.JSON("play: time", sky))
	mux.HandleFunc("GET /api/activity", site.JSON("play: activity", feedCache))
}
