// Package site holds the HTTP pieces the web apps share: a small cache for
// expensive API values, a JSON handler for them, and the static file server
// that also serves the stylesheet shared between sites.
package site

import (
	"context"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"path"
	"sync"
	"time"
)

// Cache holds the result of Load for MaxAge, so many visitors polling a
// page cause at most one load per period. The lock is held during Load, so
// concurrent requests wait for one refresh instead of each doing the work.
type Cache[T any] struct {
	MaxAge time.Duration
	Load   func(context.Context) (T, error)

	mu      sync.Mutex
	value   T
	err     error
	fetched time.Time
}

// Get returns the cached value, loading it first if it is missing or stale.
func (c *Cache[T]) Get(ctx context.Context) (T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.fetched.IsZero() || time.Since(c.fetched) >= c.MaxAge {
		c.value, c.err = c.Load(ctx)
		c.fetched = time.Now()
	}
	return c.value, c.err
}

// JSON returns a handler that writes the cached value as JSON. name is
// used in log messages.
func JSON[T any](name string, c *Cache[T]) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Don't let one visitor closing the tab cancel the load: the result
		// is cached and shared, and a cancelled ping would be cached as
		// "offline".
		v, err := c.Get(context.WithoutCancel(r.Context()))
		if err != nil {
			log.Printf("%s: %v", name, err)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		WriteJSON(w, name, v)
	}
}

// WriteJSON writes v as an uncached JSON response.
func WriteJSON(w http.ResponseWriter, name string, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writing %s: %v", name, err)
	}
}

// Static serves the site's files from dir, and the files every site
// shares from sharedDir under /shared/. Folders without an index.html are
// 404s rather than file listings.
//
// Every response is no-cache: a release replaces these files in place, so
// Cloudflare and browsers must check with us before reusing a copy, or a
// cached app.js from the last release runs against the new index.html.
func Static(dir, sharedDir string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /shared/", http.StripPrefix("/shared/", http.FileServer(noListings{http.Dir(sharedDir)})))
	mux.Handle("GET /", http.FileServer(noListings{http.Dir(dir)}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		mux.ServeHTTP(w, r)
	})
}

// noListings hides folders that have no index.html, so http.FileServer
// answers 404 instead of listing what's inside.
type noListings struct{ fs http.FileSystem }

func (n noListings) Open(name string) (http.File, error) {
	f, err := n.fs.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if info.IsDir() {
		index, err := n.fs.Open(path.Join(name, "index.html"))
		if err != nil {
			f.Close()
			return nil, fs.ErrNotExist
		}
		index.Close()
	}
	return f, nil
}
