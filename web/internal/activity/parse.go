package activity

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Event kinds.
const (
	Join        = "join"
	Leave       = "leave"
	Death       = "death"
	Advancement = "advancement"
)

// maxText bounds an event's text. Death messages can carry a mob's name
// tag, which players choose.
const maxText = 160

// line is one parsed log line: the clock time it was written, in seconds
// after midnight, and what it says.
type line struct {
	clock int
	kind  string // an event kind, a lifecycle kind below, or "" for anything else
	name  string // the player
	text  string // the event's message, for deaths
	title string // the advancement's title
}

// Server lifecycle lines, which reset who is online.
const (
	started = "started"
	stopped = "stopped"
)

// stamp matches the time every line of Paper's file log starts with:
//
//	[12:34:56] [Server thread/INFO]: Alice joined the game
var stamp = regexp.MustCompile(`^\[([01]\d|2[0-3]):([0-5]\d):([0-5]\d)\] `)

// fromGame starts the lines the game itself writes on the server thread.
// Chat is logged from the async chat thread and plugins' lines carry a
// "[plugin]" prefix, so nothing a player types can pass for an event.
const fromGame = "[Server thread/INFO]: "

// playerName matches a Java Edition name, or a Bedrock one through
// Floodgate's default "." prefix.
var playerName = regexp.MustCompile(`^\.?[A-Za-z0-9_]{1,16}$`)

// deathPrefixes are the starts of every vanilla death message after the
// player's name ("Alice <prefix>..."), from the game's en_us.json, reduced
// so none is an extension of another.
var deathPrefixes = []string{
	"blew up",
	"burned to death",
	"didn't want to live in the same world as",
	"died",
	"discovered the floor was lava",
	"drowned",
	"experienced kinetic energy",
	"fell from a high place",
	"fell off a ladder",
	"fell off scaffolding",
	"fell off some twisting vines",
	"fell off some vines",
	"fell off some weeping vines",
	"fell out of the world",
	"fell too far and was finished by",
	"fell while climbing",
	"froze to death",
	"hit the ground too hard",
	"left the confines of this world",
	"starved to death",
	"suffocated in a wall",
	"tried to swim in lava",
	"walked into a cactus while trying to escape",
	"walked into fire while fighting",
	"walked into the danger zone due to",
	"was blown up by",
	"was burned to a crisp while fighting",
	"was doomed to fall",
	"was fireballed by",
	"was frozen to death by",
	"was impaled by",
	"was impaled on a stalagmite",
	"was killed",
	"was obliterated by a sonically-charged shriek",
	"was poked to death by a sweet berry bush",
	"was pricked to death",
	"was pummeled by",
	"was roasted in dragon's breath",
	"was shot by",
	"was skewered by a falling stalactite",
	"was slain by",
	"was smashed by",
	"was speared by",
	"was squashed by",
	"was squished too much",
	"was struck by lightning",
	"was stung to death",
	"went off with a bang",
	"went up in flames",
	"withered away",
}

// advancementVerbs are how the game announces each advancement frame.
var advancementVerbs = []string{
	"has made the advancement ",
	"has completed the challenge ",
	"has reached the goal ",
}

// parseLine reads one log line. ok is false for lines without a time
// stamp (such as stack traces). Lines that aren't events or lifecycle
// markers, which is nearly all of them, have an empty kind.
func parseLine(s string) (l line, ok bool) {
	m := stamp.FindStringSubmatch(s)
	if m == nil {
		return line{}, false
	}
	l.clock = atoi2(m[1])*3600 + atoi2(m[2])*60 + atoi2(m[3])
	msg, ok := strings.CutPrefix(s[len(m[0]):], fromGame)
	if !ok {
		return l, true
	}

	switch {
	case strings.HasPrefix(msg, "Done (") && strings.Contains(msg, `For help, type "help"`):
		l.kind = started
		return l, true
	case msg == "Stopping server" || msg == "Stopping the server":
		l.kind = stopped
		return l, true
	}

	name, rest, found := strings.Cut(msg, " ")
	if !found || !playerName.MatchString(name) {
		return l, true
	}

	switch {
	case rest == "joined the game":
		l.kind, l.name = Join, name
	case strings.HasPrefix(rest, "(formerly known as ") && strings.HasSuffix(rest, ") joined the game"):
		l.kind, l.name = Join, name
	case rest == "left the game":
		l.kind, l.name = Leave, name
	case isDeath(rest):
		l.kind, l.name, l.text = Death, name, clean(msg)
	default:
		for _, verb := range advancementVerbs {
			title, ok := strings.CutPrefix(rest, verb)
			if !ok {
				continue
			}
			title, ok = strings.CutPrefix(title, "[")
			if !ok {
				break
			}
			title, ok = strings.CutSuffix(title, "]")
			if title = clean(title); !ok || title == "" {
				break
			}
			l.kind, l.name, l.title = Advancement, name, title
			break
		}
	}
	return l, true
}

func isDeath(rest string) bool {
	for _, p := range deathPrefixes {
		if rest == p || strings.HasPrefix(rest, p+" ") {
			return true
		}
	}
	return false
}

// clean drops control and format characters and invalid UTF-8, and cuts
// the text to maxText characters.
func clean(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		if n == maxText {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// atoi2 parses two ASCII digits, which the header regexp guarantees.
func atoi2(s string) int {
	return int(s[0]-'0')*10 + int(s[1]-'0')
}
