package main

// Release control (#77) and update policies (#83, #84). admin never deploys
// anything itself: a deploy restarts admin partway through. It writes the
// change to mc-agent's request file and starts mc-agent.service, and the
// agent carries it out under its own lock.

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/deploy"
	"github.com/ryancaudill04/minecraft-server/web/internal/site"
)

// TrackSettings is one track's row in GET /api/settings.
type TrackSettings struct {
	Track     string      `json:"track"`
	Name      string      `json:"name"`
	Active    string      `json:"active"`              // "" if nothing is active
	Available string      `json:"available,omitzero"`  // the newest release mc-agent saw
	CheckedAt time.Time   `json:"checked_at,omitzero"` // when it looked
	Mode      deploy.Mode `json:"mode"`
	Tag       string      `json:"tag,omitzero"` // the pinned release
}

// Change is a change waiting for, or carried out by, mc-agent. It leaves
// out who asked: that's in admin's audit log.
type Change struct {
	Action string    `json:"action"`
	Tag    string    `json:"tag,omitzero"`
	At     time.Time `json:"at"`
	Done   time.Time `json:"done_at,omitzero"`
	Error  string    `json:"error,omitzero"`
}

// Settings is the JSON body of GET /api/settings.
type Settings struct {
	Paused  bool            `json:"paused"`
	Tracks  []TrackSettings `json:"tracks"`
	Pending *Change         `json:"pending,omitzero"` // waiting for mc-agent
	Last    *Change         `json:"last,omitzero"`    // what mc-agent did with the last one
	// CanChange says whether this user can change anything, and Locked
	// why not.
	CanChange bool   `json:"can_change"`
	Locked    string `json:"locked,omitzero"`
}

// ReleaseRow is one release in GET /api/releases.
type ReleaseRow struct {
	Tag         string    `json:"tag"`
	PublishedAt time.Time `json:"published_at,omitzero"`
	URL         string    `json:"url,omitzero"` // its page on GitHub, with the notes
	Installed   bool      `json:"installed"`
	Active      bool      `json:"active"`
	Pinned      bool      `json:"pinned"`
	Failed      bool      `json:"failed"`
	// Selectable says whether it can be pinned, and Reason why not.
	Selectable bool   `json:"selectable"`
	Reason     string `json:"reason,omitzero"`
}

// ReleaseList is the JSON body of GET /api/releases.
type ReleaseList struct {
	Releases    []ReleaseRow `json:"releases"`
	GitHubError string       `json:"github_error,omitzero"`
}

const (
	lockedPolicy = "The update policy was written by a newer mc-agent, so admin can only show it."
	tooOld       = "From before update policies: its mc-agent would ignore the pin and deploy the latest release."
	unknownAge   = "Can't tell whether it supports update policies until a release that does is installed."
)

// published is what the GitHub release list cache holds. A failed lookup
// is cached too, so a GitHub outage doesn't make every page load wait.
type published struct {
	releases []deploy.Release
	err      error
}

// pendingChange returns the request waiting for mc-agent, if one is.
func pendingChange(agent *deploy.Deployer) *Change {
	p, err := agent.PendingRequest()
	if err != nil {
		log.Printf("admin: pending request: %v", err)
		return nil
	}
	if p == nil {
		return nil
	}
	return &Change{Action: p.Action, Tag: p.Tag, At: p.At}
}

func (a *app) settings(w http.ResponseWriter, r *http.Request) {
	res := Settings{CanChange: true}
	if a.user(r) == "" {
		res.CanChange, res.Locked = false, lockedNoUser
	}
	active, err := a.agent.Current()
	if err != nil {
		log.Printf("admin: settings: %v", err)
		writeError(w, http.StatusServiceUnavailable, "Couldn't read the deploy state.")
		return
	}
	row := TrackSettings{Track: deploy.Track, Name: "All apps", Active: active, Mode: deploy.ModeAuto}
	switch p, err := a.agent.ReadPolicy(); {
	case errors.Is(err, deploy.ErrUnsupportedPolicy):
		res.CanChange, res.Locked = false, lockedPolicy
	case err != nil:
		log.Printf("admin: settings: %v", err)
		writeError(w, http.StatusServiceUnavailable, "Couldn't read the update policy.")
		return
	default:
		res.Paused = p.Paused
		row.Mode, row.Tag = p.Tracks[deploy.Track].Mode, p.Tracks[deploy.Track].Tag
	}
	if s, err := a.agent.LatestSeen(); err != nil {
		log.Printf("admin: settings: %v", err)
	} else if s != nil {
		row.Available, row.CheckedAt = s.Tag, s.Checked
	}
	res.Tracks = []TrackSettings{row}
	res.Pending = pendingChange(a.agent)
	if l, err := a.agent.LastRequest(); err != nil {
		log.Printf("admin: settings: %v", err)
	} else if l != nil {
		res.Last = &Change{Action: l.Action, Tag: l.Tag, At: l.At, Done: l.Done, Error: l.Error}
	}
	site.WriteJSON(w, "admin: settings", res)
}

func (a *app) releases(w http.ResponseWriter, r *http.Request) {
	rows, ghErr, err := a.releaseRows(r.Context())
	if err != nil {
		log.Printf("admin: releases: %v", err)
		writeError(w, http.StatusServiceUnavailable, "Couldn't read the installed releases.")
		return
	}
	res := ReleaseList{Releases: rows}
	if ghErr != nil {
		res.GitHubError = "Couldn't ask GitHub for releases, so only installed ones are listed."
	}
	site.WriteJSON(w, "admin: releases", res)
}

// releaseRows merges GitHub's releases, newest first, with the installed
// ones, and works out which can be pinned. ghErr is GitHub's error, if it
// failed; only installed releases are listed then.
func (a *app) releaseRows(ctx context.Context) (rows []ReleaseRow, ghErr, err error) {
	installed, err := a.agent.InstalledReleases()
	if err != nil {
		return nil, nil, err
	}
	active, err := a.agent.Current()
	if err != nil {
		return nil, nil, err
	}
	pinned := ""
	if p, err := a.agent.ReadPolicy(); err == nil && p.Tracks[deploy.Track].Mode == deploy.ModePinned {
		pinned = p.Tracks[deploy.Track].Tag
	}
	pub, _ := a.github.Get(context.WithoutCancel(ctx))
	if pub.err != nil {
		log.Printf("admin: releases: %v", pub.err)
	}

	byTag := map[string]deploy.InstalledRelease{}
	for _, r := range installed {
		byTag[r.Tag] = r
	}
	// The oldest release known to follow update policies. GitHub's newer
	// releases do too: support is never removed.
	var floor time.Time
	for _, r := range pub.releases {
		if byTag[r.Tag].Policy && !r.PublishedAt.IsZero() {
			floor = r.PublishedAt
		}
	}

	row := func(tag string) ReleaseRow {
		inst, ok := byTag[tag]
		return ReleaseRow{
			Tag:       tag,
			URL:       "https://github.com/" + a.repo + "/releases/tag/" + url.PathEscape(tag),
			Installed: ok,
			Active:    tag == active,
			Pinned:    tag == pinned,
			Failed:    inst.Failed,
		}
	}
	seen := map[string]bool{}
	for _, r := range pub.releases {
		x := row(r.Tag)
		x.PublishedAt = r.PublishedAt
		switch {
		case x.Installed:
			x.Selectable = byTag[r.Tag].Policy
		case floor.IsZero():
			x.Reason = unknownAge
		default:
			x.Selectable = !r.PublishedAt.Before(floor)
		}
		if !x.Selectable && x.Reason == "" {
			x.Reason = tooOld
		}
		rows = append(rows, x)
		seen[r.Tag] = true
	}
	// Installed releases GitHub didn't list: deleted, or older than its
	// first page.
	for _, inst := range installed {
		if seen[inst.Tag] {
			continue
		}
		x := row(inst.Tag)
		x.Selectable = inst.Policy
		if !x.Selectable {
			x.Reason = tooOld
		}
		rows = append(rows, x)
	}
	return rows, pub.err, nil
}

// PolicyChange is the JSON body of POST /api/settings/{track}.
type PolicyChange struct {
	Mode deploy.Mode `json:"mode"`          // auto, notify or pinned
	Tag  string      `json:"tag,omitempty"` // for pinned: the release to apply
}

// setPolicy handles POST /api/settings/{track}: follow the latest release,
// notify only, or apply a release and stay on it.
func (a *app) setPolicy(w http.ResponseWriter, r *http.Request, user string) {
	if r.PathValue("track") != deploy.Track {
		writeError(w, http.StatusNotFound, "No such track.")
		return
	}
	var c PolicyChange
	if err := decode(r, &c); err != nil {
		writeError(w, http.StatusBadRequest, "Couldn't read the change.")
		return
	}
	req := deploy.Request{Track: deploy.Track, By: user, At: time.Now().UTC()}
	switch c.Mode {
	case deploy.ModeAuto:
		req.Action = deploy.ActionAuto
	case deploy.ModeNotify:
		req.Action = deploy.ActionNotify
	case deploy.ModePinned:
		req.Action, req.Tag = deploy.ActionPin, c.Tag
	default:
		writeError(w, http.StatusBadRequest, "The policy must be auto, notify or pinned.")
		return
	}
	if c.Mode != deploy.ModePinned && c.Tag != "" {
		writeError(w, http.StatusBadRequest, "Only a pinned policy takes a release.")
		return
	}
	old, ok := a.currentPolicy(w)
	if !ok {
		return
	}
	if c.Mode == deploy.ModePinned {
		// Only releases GitHub lists or that are installed, and only ones
		// whose agent would follow the pin. mc-agent checks again.
		rows, _, err := a.releaseRows(r.Context())
		if err != nil {
			log.Printf("admin: releases: %v", err)
			writeError(w, http.StatusServiceUnavailable, "Couldn't read the installed releases.")
			return
		}
		var found *ReleaseRow
		for i := range rows {
			if rows[i].Tag == c.Tag {
				found = &rows[i]
			}
		}
		switch {
		case found == nil:
			writeError(w, http.StatusBadRequest, "No such release.")
			return
		case !found.Selectable:
			writeError(w, http.StatusBadRequest, found.Reason)
			return
		}
	}
	newPolicy := string(c.Mode)
	if c.Mode == deploy.ModePinned {
		newPolicy += " " + c.Tag
	}
	a.submit(w, r, req, "update policy for "+deploy.Track+": "+old+" -> "+newPolicy, user)
}

// Pause is the JSON body of POST /api/pause.
type Pause struct {
	Paused *bool `json:"paused"`
}

// setPaused handles POST /api/pause: pause or resume every update.
func (a *app) setPaused(w http.ResponseWriter, r *http.Request, user string) {
	var p Pause
	if err := decode(r, &p); err != nil || p.Paused == nil {
		writeError(w, http.StatusBadRequest, `Send {"paused": true} or {"paused": false}.`)
		return
	}
	if _, ok := a.currentPolicy(w); !ok {
		return
	}
	req := deploy.Request{Action: deploy.ActionResume, By: user, At: time.Now().UTC()}
	what := "resume updates"
	if *p.Paused {
		req.Action, what = deploy.ActionPause, "pause updates"
	}
	a.submit(w, r, req, what, user)
}

// currentPolicy describes the policy for the audit log, or answers with an
// error and false if it can't be changed.
func (a *app) currentPolicy(w http.ResponseWriter) (string, bool) {
	p, err := a.agent.ReadPolicy()
	if errors.Is(err, deploy.ErrUnsupportedPolicy) {
		writeError(w, http.StatusConflict, lockedPolicy)
		return "", false
	}
	if err != nil {
		log.Printf("admin: policy: %v", err)
		writeError(w, http.StatusServiceUnavailable, "Couldn't read the update policy.")
		return "", false
	}
	tp := p.Tracks[deploy.Track]
	s := string(tp.Mode)
	if tp.Tag != "" {
		s += " " + tp.Tag
	}
	if p.Paused {
		s += " (paused)"
	}
	return s, true
}

// submit hands req to mc-agent, logs what for the audit log, and starts
// the agent so it doesn't wait for the timer.
func (a *app) submit(w http.ResponseWriter, r *http.Request, req deploy.Request, what, user string) {
	a.submitMu.Lock()
	err := a.agent.Submit(req)
	a.submitMu.Unlock()
	switch {
	case errors.Is(err, deploy.ErrBusy):
		writeError(w, http.StatusConflict, "mc-agent hasn't carried out the last change yet. Try again when it's done.")
		return
	case err != nil:
		log.Printf("admin: submitting %s: %v", what, err)
		writeError(w, http.StatusInternalServerError, "Couldn't hand the change to mc-agent: see admin's log.")
		return
	}
	log.Printf("admin: audit: %s: %s", user, what)

	// Finish asking systemd even if the browser goes away.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
	defer cancel()
	started := true
	if err := a.startAgent(ctx); err != nil {
		// The timer picks the request up within a few minutes anyway.
		log.Printf("admin: starting mc-agent.service: %v", err)
		started = false
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]bool{"started": started})
}

// decode reads one JSON object from r's body into v, refusing unknown
// fields and anything after it.
func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}

// startAgent runs "systemctl --user start --no-block mc-agent.service",
// with fixed arguments and no shell, so the agent carries out a request
// now instead of on the timer's next run.
func startAgent(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "systemctl", "--user", "start", "--no-block", "mc-agent.service").CombinedOutput()
	if err != nil {
		return errors.New(err.Error() + ": " + strings.TrimSpace(string(out)))
	}
	return nil
}
