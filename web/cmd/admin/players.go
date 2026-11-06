package main

// Players: the whitelist and the ops list. Both are read from the server
// folder, so they show while Paper sleeps, and changed only over RCON, so
// Paper looks up each name's UUID and saves the file itself.

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/ryancaudill04/minecraft-server/web/internal/rcon"
	"github.com/ryancaudill04/minecraft-server/web/internal/site"
)

// Listed is one player on the whitelist.
type Listed struct {
	Name string `json:"name"`
	UUID string `json:"uuid"`
}

// Op is one player on the ops list.
type Op struct {
	Name  string `json:"name"`
	UUID  string `json:"uuid"`
	Level int    `json:"level"` // 1 to 4; 4 can run every command
}

// Players is the JSON body of GET /api/players. WhiteList and Enforce are
// server.properties' white-list and enforce-whitelist, missing if the file
// or the key is.
type Players struct {
	Whitelist []Listed `json:"whitelist"`
	Ops       []Op     `json:"ops"`
	WhiteList *bool    `json:"white_list,omitempty"`
	Enforce   *bool    `json:"enforce_whitelist,omitempty"`
}

// PlayerName is the JSON body of POST /api/players/{list}/{action}.
type PlayerName struct {
	Name string `json:"name"`
}

// validName matches a Minecraft Java username. Nothing else gets near an
// RCON command, so a name can't add a second argument or a second command.
var validName = regexp.MustCompile(`^[A-Za-z0-9_]{3,16}$`)

// playerCommands maps {list}/{action} to the console command it runs.
var playerCommands = map[string]string{
	"whitelist/add":    "whitelist add",
	"whitelist/remove": "whitelist remove",
	"ops/add":          "op",
	"ops/remove":       "deop",
}

// readPlayers reads whitelist.json, ops.json and server.properties from
// serverDir. A missing file is an empty list (Paper writes them on its
// first start); a file that isn't valid JSON is an error.
func readPlayers(serverDir string) (Players, error) {
	p := Players{Whitelist: []Listed{}, Ops: []Op{}}
	if err := readJSONList(filepath.Join(serverDir, "whitelist.json"), &p.Whitelist); err != nil {
		return Players{}, err
	}
	if err := readJSONList(filepath.Join(serverDir, "ops.json"), &p.Ops); err != nil {
		return Players{}, err
	}
	slices.SortFunc(p.Whitelist, func(a, b Listed) int { return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	slices.SortFunc(p.Ops, func(a, b Op) int { return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })

	props, err := readProperties(filepath.Join(serverDir, "server.properties"), "white-list", "enforce-whitelist")
	if err != nil {
		return Players{}, err
	}
	p.WhiteList, p.Enforce = props["white-list"], props["enforce-whitelist"]
	return p, nil
}

func readJSONList[T any](path string, list *[]T) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, list); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if *list == nil { // the file said null
		*list = []T{}
	}
	return nil
}

// readProperties returns the true/false values of keys in a
// server.properties file. Only the keys asked for are kept: the file
// also holds rcon.password. A missing file, a missing key or a value
// that isn't true or false is left out.
func readProperties(path string, keys ...string) (map[string]*bool, error) {
	out := map[string]*bool{}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == '!' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !slices.Contains(keys, k) || (v != "true" && v != "false") {
			continue
		}
		b := v == "true"
		out[k] = &b
	}
	return out, sc.Err()
}

// players handles GET /api/players.
func (a *app) players(w http.ResponseWriter, r *http.Request, user string) {
	p, err := readPlayers(a.serverDir)
	if err != nil {
		log.Printf("admin: players: %v", err)
		writeError(w, http.StatusServiceUnavailable, "Couldn't read the whitelist or ops list.")
		return
	}
	site.WriteJSON(w, "admin: players", p)
}

// changePlayer handles POST /api/players/{list}/{action} with
// {"name": "..."}: whitelist add or remove, op or deop, over RCON.
//
// While Paper sleeps RCON doesn't answer, and this refuses with 503
// rather than editing the files: Paper would need restarting to see the
// change, and a new entry needs the player's UUID, which Paper looks up.
func (a *app) changePlayer(w http.ResponseWriter, r *http.Request, user string) {
	verb, ok := playerCommands[r.PathValue("list")+"/"+r.PathValue("action")]
	if !ok {
		writeError(w, http.StatusNotFound, "No such action.")
		return
	}
	if a.rcon == nil {
		writeError(w, http.StatusServiceUnavailable, lockedNoRCON)
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "Send JSON.")
		return
	}
	var p PlayerName
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "Couldn't read the name.")
		return
	}
	if !validName.MatchString(p.Name) {
		writeError(w, http.StatusBadRequest, "A Minecraft name is 3 to 16 letters, digits or underscores.")
		return
	}
	cmd := verb + " " + p.Name
	log.Printf("admin: audit: %s: %s", user, cmd)

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), rconTimeout)
	defer cancel()
	out, err := a.rcon(ctx, cmd)
	switch {
	case errors.Is(err, rcon.ErrUnreachable):
		writeError(w, http.StatusServiceUnavailable, "Paper is asleep, so the list can't change. Wake it from the Server section.")
	case errors.Is(err, rcon.ErrAuth):
		log.Printf("admin: players: RCON refused the password")
		writeError(w, http.StatusBadGateway, "RCON refused admin's password. Check that RCON_PASSWORD matches rcon.password.")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "RCON didn't answer in time.")
	case err != nil:
		log.Printf("admin: players: %v", err)
		writeError(w, http.StatusBadGateway, "RCON failed: see admin's log.")
	default:
		// Paper answers in words ("Added steve to the whitelist", "Player
		// is already whitelisted", "That player does not exist"), which
		// the page shows as they are.
		site.WriteJSON(w, "admin: players", Output{Output: stripFormatting(out)})
	}
}
