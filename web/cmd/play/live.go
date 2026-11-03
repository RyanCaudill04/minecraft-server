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

// skyState is what /api/time works the sky out from.
type skyState struct {
	saved  *world.Sky
	active bool      // Paper is awake
	since  time.Time // when the log says it started, if it does
}

// addLive adds the routes that follow the running game: the clock and
// weather run forward from the last save (GET /api/time), and the feed of
// joins, leaves, deaths and advancements from the server's log
// (GET /api/activity).
//
// The visits the server list named players during are kept in visible.json
// under $STATE_DIRECTORY (set by systemd's StateDirectory=) when it's set,
// so the feed's history survives restarts.
//
// It returns poll, which reads the log and the server list the way a
// request does. Run it on a timer (see watch): a visit is only shown once
// the server list has named the player during it, and nobody may have the
// page open while they play.
func addLive(mux *http.ServeMux, serverDir string, status *site.Cache[mcstatus.Status]) (poll func(context.Context)) {
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
			for _, p := range s.Players {
				if p.ID != anonymousID {
					names = append(names, p.Name)
				}
			}
			if err := feed.Observe(names); err != nil {
				log.Printf("play: activity: %v", err)
			}
		}
		return s
	}

	worldDir := filepath.Join(serverDir, "world")
	// The cache holds what takes work to find; At is cheap, so each answer
	// is worked out for the moment it's sent. Otherwise a cached answer is
	// up to 5s behind and the page's clock steps back when a fresh one
	// follows a stale one.
	sky := &site.Cache[skyState]{MaxAge: 5 * time.Second, Load: func(ctx context.Context) (skyState, error) {
		s := update(ctx)
		saved, err := world.ReadSky(worldDir)
		if err != nil {
			return skyState{}, err
		}
		since, running := feed.Running()
		if !running {
			since = time.Time{}
		}
		return skyState{saved, s.State == mcstatus.StateActive, since}, nil
	}}
	feedCache := &site.Cache[Activity]{MaxAge: 5 * time.Second, Load: func(ctx context.Context) (Activity, error) {
		update(ctx)
		return Activity{Events: feed.Events(feedSize)}, nil
	}}

	mux.HandleFunc("GET /api/time", func(w http.ResponseWriter, r *http.Request) {
		st, err := sky.Get(context.WithoutCancel(r.Context()))
		if err != nil {
			log.Printf("play: time: %v", err)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		site.WriteJSON(w, "play: time", st.saved.At(time.Now(), st.active, st.since))
	})
	mux.HandleFunc("GET /api/activity", site.JSON("play: activity", feedCache))
	return func(ctx context.Context) { update(ctx) }
}

// watch calls poll every interval until ctx is done.
func watch(ctx context.Context, interval time.Duration, poll func(context.Context)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
