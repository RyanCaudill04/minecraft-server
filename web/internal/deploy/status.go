package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Outcomes of a deploy, as recorded in Result.
const (
	OutcomeSuccess    = "success"     // the release is active and healthy
	OutcomeFailed     = "failed"      // it didn't install or activate, and nothing was rolled back
	OutcomeRolledBack = "rolled_back" // it failed and the previous release is active again
)

// Result is the outcome of the last deploy or rollback. It is
// <apps>/result.json.
type Result struct {
	Tag          string    `json:"tag"`     // the release that was deployed
	Outcome      string    `json:"outcome"` // OutcomeSuccess, OutcomeFailed or OutcomeRolledBack
	At           time.Time `json:"at"`
	Error        string    `json:"error,omitzero"`          // why it failed
	RolledBackTo string    `json:"rolled_back_to,omitzero"` // for OutcomeRolledBack
}

// Status is a snapshot of what the agent has deployed. Everything but
// Latest is read from Apps, so it works without the network.
type Status struct {
	Active     string    `json:"active"`              // release tag, or "" if none is active
	AppliedAt  time.Time `json:"applied_at,omitzero"` // when current was last switched to it
	Failed     string    `json:"failed,omitzero"`     // a release that won't be retried
	Installed  []string  `json:"installed"`           // newest first
	LastResult *Result   `json:"last_result,omitzero"`

	// Latest is the newest release on GitHub, or "" if it wasn't checked
	// or the check failed (see LatestError).
	Latest      string `json:"latest,omitzero"`
	LatestError string `json:"latest_error,omitzero"`

	// Warnings name records that couldn't be read. Their fields are left
	// empty rather than failing the whole status.
	Warnings []string `json:"warnings,omitzero"`
}

// Behind reports whether GitHub has a newer release than the active one.
func (s *Status) Behind() bool { return s.Latest != "" && s.Latest != s.Active }

// latestTimeout bounds the GitHub lookup in Status, so status stays quick
// when the network is down.
const latestTimeout = 5 * time.Second

func (d *Deployer) resultFile() string { return filepath.Join(d.Apps, "result.json") }

// Status reports the active, failed and installed releases and the last
// deploy's result. It only reads, so it creates nothing in Apps. If API is
// set it also asks GitHub for the newest release; a failed lookup is noted
// in LatestError, never returned. It fails only if the active release or
// the installed ones can't be read.
func (d *Deployer) Status(ctx context.Context) (*Status, error) {
	var s Status
	var err error
	if s.Active, err = d.Current(); err != nil {
		return nil, err
	}
	if s.Installed, err = d.installed(); errors.Is(err, fs.ErrNotExist) {
		s.Installed = []string{}
	} else if err != nil {
		return nil, err
	}

	warn := func(err error) { s.Warnings = append(s.Warnings, err.Error()) }
	if a, err := d.LastApplied(); err != nil {
		warn(err)
	} else if a != nil {
		s.AppliedAt = a.At
	}
	if data, err := os.ReadFile(d.failedFile()); err == nil {
		s.Failed = strings.TrimSpace(string(data))
	} else if !errors.Is(err, fs.ErrNotExist) {
		warn(err)
	}
	if s.LastResult, err = d.LastResult(); err != nil {
		warn(err)
	}

	if d.API != "" && d.HTTP != nil {
		ctx, cancel := context.WithTimeout(ctx, latestTimeout)
		defer cancel()
		if rel, err := d.Latest(ctx); err != nil {
			s.LatestError = err.Error()
		} else {
			s.Latest = rel.Tag
		}
	}
	return &s, nil
}

// LastResult returns the last deploy's result, or nil if none is recorded.
func (d *Deployer) LastResult() (*Result, error) {
	data, err := os.ReadFile(d.resultFile())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Result
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("deploy: reading %s: %w", d.resultFile(), err)
	}
	return &r, nil
}

// recordResult writes the outcome of deploying tag to result.json:
// success if err is nil, a rollback if rolledBackTo is set, else a
// failure. Like active.json it's only a record, so failing to write it is
// logged, not returned.
func (d *Deployer) recordResult(tag, rolledBackTo string, err error) {
	r := Result{Tag: tag, Outcome: OutcomeSuccess, At: time.Now().UTC()}
	if err != nil {
		r.Outcome, r.Error = OutcomeFailed, err.Error()
		if rolledBackTo != "" {
			r.Outcome, r.RolledBackTo = OutcomeRolledBack, rolledBackTo
		}
	}
	data, merr := json.Marshal(r)
	if merr == nil {
		merr = d.writeFile(d.resultFile(), data)
	}
	if merr != nil {
		d.Log.Printf("recording the result of %s: %v", tag, merr)
	}
}
