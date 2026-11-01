package activity

import (
	"strings"
	"testing"
)

func TestParseLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
		ok   bool // has a time stamp
		want line
	}{
		{"join", "[12:34:56] [Server thread/INFO]: Alice joined the game", true,
			line{clock: 12*3600 + 34*60 + 56, kind: Join, name: "Alice"}},
		{"renamed join", "[00:00:01] [Server thread/INFO]: Alice (formerly known as Al1ce) joined the game", true,
			line{clock: 1, kind: Join, name: "Alice"}},
		{"leave", "[23:59:59] [Server thread/INFO]: Bob_2 left the game", true,
			line{clock: 86399, kind: Leave, name: "Bob_2"}},
		{"bedrock name", "[10:00:00] [Server thread/INFO]: .Steve joined the game", true,
			line{clock: 36000, kind: Join, name: ".Steve"}},
		{"death by mob", "[10:00:00] [Server thread/INFO]: Alice was slain by Zombie", true,
			line{clock: 36000, kind: Death, name: "Alice", text: "Alice was slain by Zombie"}},
		{"death alone", "[10:00:00] [Server thread/INFO]: Alice drowned", true,
			line{clock: 36000, kind: Death, name: "Alice", text: "Alice drowned"}},
		{"death with item", "[10:00:00] [Server thread/INFO]: Alice was shot by Skeleton using [Bow of Doom]", true,
			line{clock: 36000, kind: Death, name: "Alice", text: "Alice was shot by Skeleton using [Bow of Doom]"}},
		{"died", "[10:00:00] [Server thread/INFO]: Alice died", true,
			line{clock: 36000, kind: Death, name: "Alice", text: "Alice died"}},
		{"advancement", "[10:00:00] [Server thread/INFO]: Alice has made the advancement [Stone Age]", true,
			line{clock: 36000, kind: Advancement, name: "Alice", title: "Stone Age"}},
		{"challenge", "[10:00:00] [Server thread/INFO]: Alice has completed the challenge [Return to Sender]", true,
			line{clock: 36000, kind: Advancement, name: "Alice", title: "Return to Sender"}},
		{"goal", "[10:00:00] [Server thread/INFO]: Alice has reached the goal [Sky's the Limit]", true,
			line{clock: 36000, kind: Advancement, name: "Alice", title: "Sky's the Limit"}},
		{"started", `[09:00:00] [Server thread/INFO]: Done (12.345s)! For help, type "help"`, true,
			line{clock: 32400, kind: started}},
		{"stopping", "[09:00:00] [Server thread/INFO]: Stopping server", true,
			line{clock: 32400, kind: stopped}},

		// Lines that must not become events.
		{"chat", "[10:00:00] [Async Chat Thread - #0/INFO]: <Alice> Bob joined the game", true, line{clock: 36000}},
		{"chat on server thread", "[10:00:00] [Server thread/INFO]: <Alice> Bob joined the game", true, line{clock: 36000}},
		{"say", "[10:00:00] [Server thread/INFO]: [Alice] Bob left the game", true, line{clock: 36000}},
		{"me", "[10:00:00] [Server thread/INFO]: * Alice drowned", true, line{clock: 36000}},
		{"command", "[10:00:00] [Server thread/INFO]: Alice issued server command: /me joined the game", true, line{clock: 36000}},
		{"plugin", "[10:00:00] [Server thread/INFO]: [spark] Alice joined the game", true, line{clock: 36000}},
		{"warning", "[10:00:00] [Server thread/WARN]: Alice joined the game", true, line{clock: 36000}},
		{"login detail", "[10:00:00] [Server thread/INFO]: Alice[/127.0.0.1:50000] logged in with entity id 7 at ([world]0.5, 64.0, 0.5)", true, line{clock: 36000}},
		{"lost connection", "[10:00:00] [Server thread/INFO]: Alice lost connection: Disconnected", true, line{clock: 36000}},
		{"name too long", "[10:00:00] [Server thread/INFO]: Abcdefghijklmnopq joined the game", true, line{clock: 36000}},
		{"name with markup", "[10:00:00] [Server thread/INFO]: <img/src=x>Alice joined the game", true, line{clock: 36000}},
		{"name with unicode", "[10:00:00] [Server thread/INFO]: Аlice joined the game", true, line{clock: 36000}},
		{"almost a death", "[10:00:00] [Server thread/INFO]: Alice diedx", true, line{clock: 36000}},
		{"trailing text", "[10:00:00] [Server thread/INFO]: Alice joined the game!", true, line{clock: 36000}},
		{"advancement without brackets", "[10:00:00] [Server thread/INFO]: Alice has made the advancement Stone Age", true, line{clock: 36000}},
		{"empty advancement", "[10:00:00] [Server thread/INFO]: Alice has made the advancement []", true, line{clock: 36000}},
		{"named mob", "[10:00:00] [Server thread/INFO]: Named entity Wolf['Rex'/12, l='world', x=1.0, y=2.0, z=3.0] died: Rex was slain by Alice", true, line{clock: 36000}},
		{"stack trace", "\tat net.minecraft.server.Main.main(Main.java:1)", false, line{}},
		{"bad time", "[24:00:00] [Server thread/INFO]: Alice joined the game", false, line{}},
		{"console format", "[10:00:00 INFO]: Alice joined the game", false, line{}},
		{"empty", "", false, line{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseLine(tt.in)
			if ok != tt.ok || got != tt.want {
				t.Errorf("parseLine(%q) = %+v, %v; want %+v, %v", tt.in, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestDeathMessagesKeepMobNamesSafe(t *testing.T) {
	// A mob's name tag is player-chosen: control characters, bidi
	// overrides and very long names don't survive.
	in := "[10:00:00] [Server thread/INFO]: Alice was slain by ‮Evil\x07 " + strings.Repeat("x", 500)
	got, ok := parseLine(in)
	if !ok || got.kind != Death {
		t.Fatalf("parseLine = %+v, %v; want a death", got, ok)
	}
	if strings.ContainsAny(got.text, "‮\x07") {
		t.Errorf("text %q keeps control characters", got.text)
	}
	if n := len([]rune(got.text)); n > maxText+1 {
		t.Errorf("text is %d characters; want at most %d", n, maxText+1)
	}
	if !strings.HasPrefix(got.text, "Alice was slain by Evil x") || !strings.HasSuffix(got.text, "…") {
		t.Errorf("text = %q", got.text)
	}
}
