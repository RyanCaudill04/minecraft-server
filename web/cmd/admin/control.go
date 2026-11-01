package main

// The parts of admin that change things or show private output: starting,
// stopping and restarting the server, its console, and its log.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/ryancaudill04/minecraft-server/web/internal/control"
	"github.com/ryancaudill04/minecraft-server/web/internal/rcon"
	"github.com/ryancaudill04/minecraft-server/web/internal/site"
)

// CSRFHeader must be on every POST. A page on another site can't add a
// custom header to a form post, and adding one to a fetch needs a CORS
// preflight that admin never allows.
const CSRFHeader = "X-Admin-Request"

const (
	defaultLogLines = 200
	maxLogLines     = 1000
	logTailBytes    = 256 << 10 // read at most this much of the end of the log
	maxBody         = 4 << 10
	rconTimeout     = 10 * time.Second
)

// Me is the JSON body of GET /api/me: who admin thinks is asking, and
// which controls they can use.
type Me struct {
	Email   string `json:"email,omitzero"`
	Control bool   `json:"control"` // start, stop and restart
	Console bool   `json:"console"` // RCON and the log
	// Locked says why the controls are off, if they are.
	Locked string `json:"locked,omitzero"`
}

const (
	lockedNoUser = "Locked: the Access token didn't name a user."
	lockedNoRCON = "RCON isn't configured: set RCON_PASSWORD in secrets.env."
)

func (a *app) me(r *http.Request) Me {
	m := Me{Email: a.user(r)}
	if m.Email == "" {
		m.Locked = lockedNoUser
		return m
	}
	m.Control = a.control != nil
	m.Console = a.rcon != nil
	if !m.Console {
		m.Locked = lockedNoRCON
	}
	return m
}

// signedIn wraps h so it runs only for a verified user, and passes their
// email on.
func (a *app) signedIn(h func(w http.ResponseWriter, r *http.Request, user string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := a.user(r)
		if user == "" {
			writeError(w, http.StatusForbidden, lockedNoUser)
			return
		}
		h(w, r, user)
	}
}

// writes wraps a POST handler with the checks every change needs: a
// verified user, a same-origin request (Sec-Fetch-Site or Origin, through
// http.CrossOriginProtection), the CSRF header, and a small body.
func (a *app) writes(h func(w http.ResponseWriter, r *http.Request, user string)) http.Handler {
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusForbidden, "Cross-origin request refused.")
	}))
	return cop.Handler(a.signedIn(func(w http.ResponseWriter, r *http.Request, user string) {
		if r.Header.Get(CSRFHeader) != "1" {
			writeError(w, http.StatusForbidden, "Missing the "+CSRFHeader+" header.")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		h(w, r, user)
	}))
}

// serverAction handles POST /api/server/{action}.
func (a *app) serverAction(w http.ResponseWriter, r *http.Request, user string) {
	act, ok := control.ParseAction(r.PathValue("action"))
	if !ok {
		writeError(w, http.StatusNotFound, "No such action.")
		return
	}
	if a.control == nil {
		writeError(w, http.StatusServiceUnavailable, "Server control isn't available.")
		return
	}
	log.Printf("admin: audit: %s: %s %s", user, act, control.Unit)
	// Finish asking systemd even if the browser goes away.
	err := a.control.Do(context.WithoutCancel(r.Context()), act)
	a.status.Reset()
	switch {
	case errors.Is(err, control.ErrBusy):
		writeError(w, http.StatusConflict, "The server is still starting or stopping. Try again when it's done.")
	case err != nil:
		log.Printf("admin: %s: %v", act, err)
		writeError(w, http.StatusBadGateway, "systemd refused: see admin's log.")
	default:
		// Accepted: systemd has queued it, and /api/status shows the rest.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"action": string(act)})
	}
}

// Command is the JSON body of POST /api/console.
type Command struct {
	Command string `json:"command"`
}

// Output is the JSON reply to POST /api/console.
type Output struct {
	Output string `json:"output"`
}

// console handles POST /api/console: one command over RCON.
func (a *app) console(w http.ResponseWriter, r *http.Request, user string) {
	if a.rcon == nil {
		writeError(w, http.StatusServiceUnavailable, lockedNoRCON)
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "Send JSON.")
		return
	}
	var c Command
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeError(w, http.StatusBadRequest, "Couldn't read the command.")
		return
	}
	cmd, msg := cleanCommand(c.Command)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	log.Printf("admin: audit: %s: console %q", user, cmd)

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), rconTimeout)
	defer cancel()
	out, err := a.rcon(ctx, cmd)
	switch {
	case errors.Is(err, rcon.ErrUnreachable):
		writeError(w, http.StatusServiceUnavailable, "RCON isn't answering. Paper is probably asleep; LazyMC wakes it when someone joins.")
	case errors.Is(err, rcon.ErrAuth):
		log.Printf("admin: console: RCON refused the password")
		writeError(w, http.StatusBadGateway, "RCON refused admin's password. Check that RCON_PASSWORD matches rcon.password.")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "RCON didn't answer in time.")
	case err != nil:
		log.Printf("admin: console: %v", err)
		writeError(w, http.StatusBadGateway, "RCON failed: see admin's log.")
	default:
		site.WriteJSON(w, "admin: console", Output{Output: stripFormatting(out)})
	}
}

// cleanCommand trims a command and drops a leading slash, or says what's
// wrong with it. Control characters are refused, so nothing can forge a
// line in the audit log or the server's log.
func cleanCommand(s string) (string, string) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "/")
	switch {
	case s == "":
		return "", "Type a command."
	case len(s) > rcon.MaxCommand:
		return "", "That command is too long."
	case strings.IndexFunc(s, unicode.IsControl) >= 0:
		return "", "Commands can't contain control characters."
	}
	return s, ""
}

// formatting matches Minecraft's § color and style codes.
var formatting = regexp.MustCompile("§.")

func stripFormatting(s string) string { return formatting.ReplaceAllString(s, "") }

// Logs is the JSON body of GET /api/logs.
type Logs struct {
	Lines []string `json:"lines"`
}

// logs handles GET /api/logs?lines=n: the end of Paper's latest.log.
func (a *app) logs(w http.ResponseWriter, r *http.Request, user string) {
	n := defaultLogLines
	if s := r.URL.Query().Get("lines"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 1 {
			writeError(w, http.StatusBadRequest, "lines must be a positive number.")
			return
		}
		n = min(v, maxLogLines)
	}
	lines, err := tailLines(a.logPath, n)
	if err != nil {
		log.Printf("admin: logs: %v", err)
		writeError(w, http.StatusServiceUnavailable, "Couldn't read the log.")
		return
	}
	site.WriteJSON(w, "admin: logs", Logs{Lines: lines})
}

// ansi matches terminal color codes, which Paper writes to some logs.
var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// tailLines returns the last n lines of the file at path, reading at most
// logTailBytes from its end. A missing file has no lines: Paper hasn't
// started since the logs were cleared.
func tailLines(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := max(0, info.Size()-logTailBytes)
	buf, err := io.ReadAll(io.NewSectionReader(f, start, info.Size()-start))
	if err != nil {
		return nil, err
	}
	if start > 0 { // the first line is probably cut off
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		}
	}
	text := strings.TrimRight(ansi.ReplaceAllString(string(buf), ""), "\n")
	if text == "" {
		return []string{}, nil
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines[max(0, len(lines)-n):], nil
}

// writeError writes {"error": msg}. msg is shown on the page as text.
func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
