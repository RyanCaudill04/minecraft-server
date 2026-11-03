package deploy

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStatus(t *testing.T) {
	applied := time.Date(2026, 10, 31, 22, 20, 0, 0, time.UTC)
	result := &Result{Tag: "r3", Outcome: OutcomeRolledBack, At: applied, Error: "health check failed", RolledBackTo: "r2"}

	// Each setup builds an apps folder; installed releases are given oldest
	// first.
	tests := []struct {
		name      string
		installed []string
		active    string
		files     map[string]string // other files in apps
		want      Status
	}{
		{
			name: "no release yet",
			want: Status{Installed: []string{}},
		},
		{
			name:      "active release",
			installed: []string{"r1"},
			active:    "r1",
			files:     map[string]string{"active.json": `{"tag":"r1","applied_at":"2026-10-31T22:20:00Z"}`},
			want:      Status{Active: "r1", AppliedAt: applied, Installed: []string{"r1"}},
		},
		{
			name:      "applied time of another release",
			installed: []string{"r1"},
			active:    "r1",
			files:     map[string]string{"active.json": `{"tag":"r0","applied_at":"2026-10-31T22:20:00Z"}`},
			want:      Status{Active: "r1", Installed: []string{"r1"}},
		},
		{
			name:      "failed marker",
			installed: []string{"r1", "r2"},
			active:    "r1",
			files:     map[string]string{"failed": "r2\n"},
			want:      Status{Active: "r1", Failed: "r2", Installed: []string{"r2", "r1"}},
		},
		{
			name:      "several installed",
			installed: []string{"r1", "r2", "r3", "r4"},
			active:    "r3",
			files:     map[string]string{"releases/.tmp-r5/x": "", "releases/notes.txt": ""},
			want:      Status{Active: "r3", Installed: []string{"r4", "r3", "r2", "r1"}},
		},
		{
			name:      "last result",
			installed: []string{"r2", "r3"},
			active:    "r2",
			files: map[string]string{
				"failed":      "r3\n",
				"result.json": `{"tag":"r3","outcome":"rolled_back","at":"2026-10-31T22:20:00Z","error":"health check failed","rolled_back_to":"r2"}`,
			},
			want: Status{Active: "r2", Failed: "r3", Installed: []string{"r3", "r2"}, LastResult: result},
		},
		{
			name:      "unreadable records",
			installed: []string{"r1"},
			active:    "r1",
			files:     map[string]string{"active.json": "{not json", "result.json": "{not json"},
			want:      Status{Active: "r1", Installed: []string{"r1"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apps := t.TempDir()
			for i, tag := range tt.installed {
				dir := filepath.Join(apps, "releases", tag)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				when := time.Now().Add(time.Duration(i-len(tt.installed)) * time.Minute)
				if err := os.Chtimes(dir, when, when); err != nil {
					t.Fatal(err)
				}
			}
			if tt.active != "" {
				if err := os.Symlink(filepath.Join("releases", tt.active), filepath.Join(apps, "current")); err != nil {
					t.Fatal(err)
				}
			}
			for name, body := range tt.files {
				path := filepath.Join(apps, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			d := &Deployer{Apps: apps}
			got, err := d.Status(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			warnings := got.Warnings
			got.Warnings = nil
			if !reflect.DeepEqual(*got, tt.want) {
				t.Errorf("Status() = %+v, want %+v", *got, tt.want)
			}
			if tt.name == "unreadable records" {
				if len(warnings) != 2 {
					t.Errorf("warnings = %q, want one each for active.json and result.json", warnings)
				}
			} else if len(warnings) != 0 {
				t.Errorf("warnings = %q, want none", warnings)
			}
		})
	}
}

func TestStatusCreatesNothing(t *testing.T) {
	apps := filepath.Join(t.TempDir(), "apps")
	d := &Deployer{Apps: apps}
	s, err := d.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Active != "" || len(s.Installed) != 0 {
		t.Errorf("Status() = %+v, want nothing active or installed", s)
	}
	if _, err := os.Stat(apps); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Status created %s (stat: %v)", apps, err)
	}
}

func TestStatusLatest(t *testing.T) {
	tests := []struct {
		name       string
		latest     string // "" makes GitHub answer 404
		wantLatest string
		wantBehind bool
		wantErr    bool
	}{
		{"up to date", "r1", "r1", false, false},
		{"behind", "r2", "r2", true, false},
		{"lookup fails", "", "", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.publish(t, "r1", "/ok")
			if err := h.d.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			h.github.mu.Lock()
			h.github.latest = tt.latest
			h.github.mu.Unlock()

			s, err := h.d.Status(context.Background())
			if err != nil {
				t.Fatalf("a failed lookup failed Status: %v", err)
			}
			if s.Active != "r1" || s.Latest != tt.wantLatest || s.Behind() != tt.wantBehind || (s.LatestError != "") != tt.wantErr {
				t.Errorf("Status() = %+v, want r1 active, latest %q, behind %v, error %v", s, tt.wantLatest, tt.wantBehind, tt.wantErr)
			}
		})
	}
}

func TestStatusLatestUnreachable(t *testing.T) {
	gh := httptest.NewServer(http.NotFoundHandler())
	gh.Close() // nothing listens there now
	d := &Deployer{Apps: t.TempDir(), API: gh.URL, Repo: "o/r", HTTP: http.DefaultClient, Log: log.New(io.Discard, "", 0)}
	s, err := d.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Latest != "" || s.LatestError == "" {
		t.Errorf("Status() = %+v, want no latest and an error", s)
	}
}

func TestRecordsDeployResults(t *testing.T) {
	tests := []struct {
		name    string
		first   bool // make r1 active before deploy runs
		deploy  func(t *testing.T, h *harness) error
		want    Result // At is checked separately
		wantErr string // in Result.Error; "" means no error
	}{
		{
			name: "success",
			deploy: func(t *testing.T, h *harness) error {
				h.publish(t, "r1", "/ok")
				return h.d.Run(context.Background())
			},
			want: Result{Tag: "r1", Outcome: OutcomeSuccess},
		},
		{
			name:  "rolled back",
			first: true,
			deploy: func(t *testing.T, h *harness) error {
				h.publish(t, "r2", "/bad")
				return h.d.Run(context.Background())
			},
			want:    Result{Tag: "r2", Outcome: OutcomeRolledBack, RolledBackTo: "r1"},
			wantErr: "health check",
		},
		{
			name: "failed with nothing to roll back to",
			deploy: func(t *testing.T, h *harness) error {
				h.publish(t, "r1", "/bad")
				return h.d.Run(context.Background())
			},
			want:    Result{Tag: "r1", Outcome: OutcomeFailed},
			wantErr: "nothing to roll back to",
		},
		{
			name:  "rollback failed too",
			first: true,
			deploy: func(t *testing.T, h *harness) error {
				h.failWhen = func(current string, args []string) bool { return args[0] == "restart" }
				h.publish(t, "r2", "/ok")
				return h.d.Run(context.Background())
			},
			want:    Result{Tag: "r2", Outcome: OutcomeFailed},
			wantErr: "rollback",
		},
		{
			name:  "install failed",
			first: true,
			deploy: func(t *testing.T, h *harness) error {
				h.publish(t, "r2", "/ok")
				h.github.checksums["r2"] = strings.Repeat("0", 64)
				return h.d.Run(context.Background())
			},
			want:    Result{Tag: "r2", Outcome: OutcomeFailed},
			wantErr: "checksum mismatch",
		},
		{
			name:  "rollback command",
			first: true,
			deploy: func(t *testing.T, h *harness) error {
				h.publish(t, "r2", "/ok")
				if err := h.d.Run(context.Background()); err != nil {
					t.Fatal(err)
				}
				h.age(t, "r2", time.Minute)
				h.age(t, "r1", 10*time.Minute)
				return h.d.Rollback(context.Background())
			},
			want: Result{Tag: "r1", Outcome: OutcomeSuccess},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.d.HealthTimeout = 300 * time.Millisecond
			if tt.first {
				h.publish(t, "r1", "/ok")
				if err := h.d.Run(context.Background()); err != nil {
					t.Fatal(err)
				}
			}

			before := time.Now()
			err := tt.deploy(t, h)
			after := time.Now()
			if (err != nil) != (tt.wantErr != "") {
				t.Fatalf("deploy err = %v, want an error: %v", err, tt.wantErr != "")
			}

			s, serr := h.d.Status(context.Background())
			if serr != nil {
				t.Fatal(serr)
			}
			r := s.LastResult
			if r == nil {
				t.Fatal("no result recorded")
			}
			if r.At.Before(before) || r.At.After(after) {
				t.Errorf("result at %v, want between %v and %v", r.At, before, after)
			}
			if !strings.Contains(r.Error, tt.wantErr) || (tt.wantErr == "") != (r.Error == "") {
				t.Errorf("result error = %q, want it to contain %q", r.Error, tt.wantErr)
			}
			got := *r
			got.At, got.Error = time.Time{}, ""
			if got != tt.want {
				t.Errorf("result = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestUpToDateRunKeepsTheLastResult(t *testing.T) {
	h := newHarness(t)
	h.publish(t, "r1", "/ok")
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, err := h.d.LastResult()
	if err != nil || first == nil {
		t.Fatalf("LastResult() = %v, %v", first, err)
	}
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.d.LastResult(); got == nil || !got.At.Equal(first.At) {
		t.Errorf("an up-to-date run replaced the result: %+v, want %+v", got, first)
	}
}

func TestUnwritableResultDoesntFailTheDeploy(t *testing.T) {
	h := newHarness(t)
	// A folder where result.json goes makes writing it fail.
	if err := os.Mkdir(filepath.Join(h.d.Apps, "result.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.publish(t, "r1", "/ok")
	if err := h.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.current(t); got != "r1" {
		t.Errorf("current = %q, want r1", got)
	}
}
