package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"sync"
)

// ErrNoRelease is returned (wrapped) when a repo has no published release
// for a track among its newest releases.
var ErrNoRelease = errors.New("no release published")

const (
	maxListBytes = 16 << 20
	perPage      = 100
)

// GitHub finds releases. Within one value, each repo's list is fetched at
// most once, so a run that checks every track makes one API call. It sends
// the ETag of the last list it got, and a 304 Not Modified, which GitHub
// doesn't count against the rate limit, reuses that list from Cache.
//
// Make a new GitHub for each run, so the next run asks again.
type GitHub struct {
	API   string // e.g. https://api.github.com
	HTTP  *http.Client
	Cache string // file holding the last list per repo; "" keeps none
	Log   *log.Logger

	mu    sync.Mutex
	lists map[string]listResult
}

type listResult struct {
	rels []Release
	err  error
}

// releaseCache is the Cache file. Only mc-agent reads it.
type releaseCache struct {
	Version int                   `json:"version"`
	Repos   map[string]cachedList `json:"repos"`
}

type cachedList struct {
	ETag     string    `json:"etag"`
	Releases []Release `json:"releases"`
}

// Releases returns repo's newest published releases (not drafts or
// prereleases), up to 100.
func (g *GitHub) Releases(ctx context.Context, repo string) ([]Release, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r, ok := g.lists[repo]; ok {
		return r.rels, r.err
	}
	rels, err := g.fetch(ctx, repo)
	if g.lists == nil {
		g.lists = map[string]listResult{}
	}
	g.lists[repo] = listResult{rels, err}
	return rels, err
}

func (g *GitHub) fetch(ctx context.Context, repo string) ([]Release, error) {
	cache := g.readCache()
	cached, haveCached := cache.Repos[repo]

	url := fmt.Sprintf("%s/repos/%s/releases?per_page=%d", g.API, repo, perPage)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if haveCached && cached.ETag != "" {
		req.Header.Set("If-None-Match", cached.ETag)
	}
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("deploy: listing releases: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotModified && haveCached:
		return cached.Releases, nil
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("deploy: listing releases: %s", resp.Status)
	}
	var all []struct {
		Release
		Draft      bool `json:"draft"`
		Prerelease bool `json:"prerelease"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxListBytes)).Decode(&all); err != nil {
		return nil, fmt.Errorf("deploy: decoding releases: %w", err)
	}
	rels := []Release{}
	for _, r := range all {
		if !r.Draft && !r.Prerelease {
			rels = append(rels, r.Release)
		}
	}
	if etag := resp.Header.Get("ETag"); etag != "" && g.Cache != "" {
		if cache.Repos == nil {
			cache.Repos = map[string]cachedList{}
		}
		cache.Repos[repo] = cachedList{ETag: etag, Releases: rels}
		cache.Version = 1
		if data, err := json.Marshal(cache); err == nil {
			err = writeFile(g.Cache, data)
			if err != nil {
				g.logf("saving %s: %v", g.Cache, err)
			}
		}
	}
	return rels, nil
}

func (g *GitHub) readCache() releaseCache {
	var c releaseCache
	if g.Cache == "" {
		return c
	}
	data, err := os.ReadFile(g.Cache)
	if err == nil {
		err = json.Unmarshal(data, &c)
	}
	if err != nil || c.Version != 1 {
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			g.logf("ignoring %s: %v", g.Cache, err)
		}
		return releaseCache{}
	}
	return c
}

func (g *GitHub) logf(format string, args ...any) {
	if g.Log != nil {
		g.Log.Printf(format, args...)
	}
}

// newest returns the release in rels whose tag is prefix + "r" + a number,
// with the highest number, or nil if there is none. The single release
// from before tracks has prefix "": its tags are r1, r2, ...
func newest(rels []Release, prefix string) *Release {
	pattern := regexp.MustCompile(`^` + regexp.QuoteMeta(prefix) + `r([0-9]{1,9})$`)
	var best *Release
	bestN := -1
	for i := range rels {
		m := pattern.FindStringSubmatch(rels[i].Tag)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		if n > bestN {
			best, bestN = &rels[i], n
		}
	}
	return best
}
