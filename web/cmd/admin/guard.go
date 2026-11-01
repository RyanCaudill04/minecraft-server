package main

// The checks every change goes through. This is the same approach, names
// and messages as #108's control.go, so the two copies collapse into one
// when both are merged; the identity comes from #87's Access check.

import (
	"encoding/json"
	"net/http"
	"strings"
)

// CSRFHeader must be on every POST. A page on another site can't add a
// custom header to a form post, and adding one to a fetch needs a CORS
// preflight that admin never allows.
const CSRFHeader = "X-Admin-Request"

// maxBody limits a POST's body.
const maxBody = 4 << 10

const lockedNoUser = "Locked until admin verifies the Cloudflare Access token (#27)."

// writes wraps a POST handler with the checks every change needs: a
// verified user, a same-origin request (Sec-Fetch-Site or Origin, through
// http.CrossOriginProtection), the CSRF header, a JSON body and a small
// one. Without a verified user, it refuses everything.
func (a *app) writes(h func(w http.ResponseWriter, r *http.Request, user string)) http.Handler {
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusForbidden, "Cross-origin request refused.")
	}))
	return cop.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := a.user(r)
		if user == "" {
			writeError(w, http.StatusForbidden, lockedNoUser)
			return
		}
		if r.Header.Get(CSRFHeader) != "1" {
			writeError(w, http.StatusForbidden, "Missing the "+CSRFHeader+" header.")
			return
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			writeError(w, http.StatusUnsupportedMediaType, "Send JSON.")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		h(w, r, user)
	}))
}

// writeError writes {"error": msg}. msg is shown on the page as text.
func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
