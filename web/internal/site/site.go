// Package site holds the HTTP pieces the web apps share: a small cache for
// expensive API values, a JSON handler for them, the static file server
// that also serves the stylesheet shared between sites, and the
// http.Server settings they all listen with.
package site

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
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

// Reset makes the next Get load a fresh value, for when the caller knows
// the cached one is out of date.
func (c *Cache[T]) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fetched = time.Time{}
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

// Server returns an http.Server for h on addr with the timeouts every app
// uses. Today only cloudflared connects, but without them a client that
// trickles headers or idles on keep-alive holds a goroutine and a file
// descriptor on the Pi for as long as it likes.
func Server(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// Static serves the site's files from dir, and the files every site
// shares from sharedDir under /shared/ (skipped if sharedDir is ""). Folders
// without an index.html and dotfiles are 404s rather than file listings.
// Every 404 is the site's NotFoundPage when it has one.
//
// Every response is no-cache: a release replaces these files in place, so
// Cloudflare and browsers must check with us before reusing a copy, or a
// cached app.js from the last release runs against the new index.html.
func Static(dir, sharedDir string) http.Handler {
	mux := http.NewServeMux()
	// Check for "" rather than serving http.Dir(""), which is the folder
	// the program happens to start in.
	if sharedDir != "" {
		mux.Handle("GET /shared/", http.StripPrefix("/shared/", http.FileServer(noListings{http.Dir(sharedDir)})))
	}
	mux.Handle("GET /", http.FileServer(noListings{http.Dir(dir)}))
	page := filepath.Join(dir, NotFoundPage)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		mux.ServeHTTP(&notFoundWriter{ResponseWriter: w, page: page}, r)
	})
}

// NotFoundPage is the page in a site's folder that Static answers 404s
// with. The dev sites on Cloudflare serve the same file for paths they
// have no file for (not_found_handling in dev/*.jsonc).
const NotFoundPage = "404.html"

// NotFound replies with status 404 and the HTML page in file, or with msg
// as plain text if the page can't be read.
func NotFound(w http.ResponseWriter, file, msg string) {
	body, err := os.ReadFile(file)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("404 page: %v", err)
		}
		http.Error(w, msg, http.StatusNotFound)
		return
	}
	h := w.Header()
	h.Del("Content-Length")
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	// A release replaces the page in place, like every other file.
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusNotFound)
	w.Write(body)
}

// notFoundWriter swaps the plain-text 404 that http.FileServer writes for
// the page in page. Every other response passes through.
type notFoundWriter struct {
	http.ResponseWriter
	page     string
	replaced bool
}

func (w *notFoundWriter) WriteHeader(code int) {
	if code != http.StatusNotFound || w.replaced {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	w.replaced = true
	NotFound(w.ResponseWriter, w.page, "404 page not found")
}

// Write drops the file server's own text once the page has been written.
func (w *notFoundWriter) Write(b []byte) (int, error) {
	if w.replaced {
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the real writer.
func (w *notFoundWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// noListings hides folders that have no index.html, so http.FileServer
// answers 404 instead of listing what's inside. It also hides any path
// with a part starting with ".", such as .git or .env, which no site means
// to publish.
type noListings struct{ fs http.FileSystem }

func (n noListings) Open(name string) (http.File, error) {
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") {
			return nil, fs.ErrNotExist
		}
	}
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
