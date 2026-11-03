package mcstatus

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/ping"
)

const (
	paperAddr  = "paper:25566"
	lazymcAddr = "lazymc:25565"
)

// fakeChecker answers pings from responses; an address that's missing
// fails as if nothing were listening.
func fakeChecker(responses map[string]*ping.Response) *Checker {
	return &Checker{
		PaperAddr:  paperAddr,
		LazymcAddr: lazymcAddr,
		Timeout:    time.Second,
		Ping: func(ctx context.Context, addr string) (*ping.Response, error) {
			if resp, ok := responses[addr]; ok {
				return resp, nil
			}
			return nil, errors.New("connection refused")
		},
	}
}

func response(motd string, online int, names ...string) *ping.Response {
	r := &ping.Response{
		Version:     ping.Version{Name: "Paper 26.2"},
		Players:     ping.Players{Online: online, Max: 20},
		Description: ping.Description{Text: motd},
	}
	for _, n := range names {
		r.Players.Sample = append(r.Players.Sample, ping.Player{Name: n, ID: n + "-id"})
	}
	return r
}

func TestCheckStates(t *testing.T) {
	tests := []struct {
		name      string
		responses map[string]*ping.Response
		want      Status
	}{
		{
			name: "paper answers: active",
			responses: map[string]*ping.Response{
				paperAddr:  response("A Minecraft Server", 2, "Alice", "Bob"),
				lazymcAddr: response("A Minecraft Server", 2),
			},
			want: Status{State: StateActive, Online: 2, Max: 20, Version: "Paper 26.2",
				Players: []Player{{"Alice", "Alice-id"}, {"Bob", "Bob-id"}}},
		},
		{
			name: "hidden players count but aren't listed",
			responses: map[string]*ping.Response{
				paperAddr: {Players: ping.Players{Online: 2, Max: 20, Sample: []ping.Player{
					{Name: "Alice", ID: "Alice-id"},
					{Name: "Anonymous Player", ID: AnonymousID},
				}}},
			},
			want: Status{State: StateActive, Online: 2, Max: 20, Players: []Player{{"Alice", "Alice-id"}}},
		},
		{
			name:      "only lazymc answers: sleeping",
			responses: map[string]*ping.Response{lazymcAddr: response("§7Server is sleeping\n§aJoin to wake it up", 0)},
			want:      Status{State: StateSleeping, Max: 20, Version: "Paper 26.2", Players: []Player{}},
		},
		{
			name:      "lazymc says starting",
			responses: map[string]*ping.Response{lazymcAddr: response("§eServer is Starting...", 0)},
			want:      Status{State: StateStarting, Max: 20, Version: "Paper 26.2", Players: []Player{}},
		},
		{
			name: "nothing answers: offline",
			want: Status{State: StateOffline, Players: []Player{}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fakeChecker(tt.responses).Check(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got.Checked.IsZero() {
				t.Error("Checked is not set")
			}
			got.Checked = time.Time{}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(tt.want)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("Check:\n got %s\nwant %s", gotJSON, wantJSON)
			}
		})
	}
}
