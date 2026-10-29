package access

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const aud = "app-aud"

var (
	now = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

	// Generating RSA keys is slow, so the tests share these.
	signer = mustKey()
	other  = mustKey()
)

func mustKey() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
}

// newVerifier returns a Verifier whose issuer serves signer's public key
// as "k1", and a count of how many times the keys were fetched.
func newVerifier(t *testing.T) (*Verifier, *atomic.Int32) {
	t.Helper()
	var fetches atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cdn-cgi/access/certs" {
			http.NotFound(w, r)
			return
		}
		fetches.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": "k1",
			"kty": "RSA",
			"alg": "RS256",
			"n":   base64.RawURLEncoding.EncodeToString(signer.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(signer.E)).Bytes()),
		}}})
	}))
	t.Cleanup(ts.Close)
	v := &Verifier{Issuer: ts.URL, AUD: aud, Now: func() time.Time { return now }}
	return v, &fetches
}

// sign makes a token from header and claims, signed with key.
func sign(t *testing.T, key *rsa.PrivateKey, header, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	unsigned := enc(header) + "." + enc(claims)
	digest := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func goodHeader() map[string]any { return map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"} }

func goodClaims(issuer string) map[string]any {
	return map[string]any{
		"aud":   []string{aud},
		"iss":   issuer,
		"exp":   now.Add(time.Hour).Unix(),
		"nbf":   now.Add(-time.Minute).Unix(),
		"iat":   now.Add(-time.Minute).Unix(),
		"email": "ryan@example.com",
	}
}

func TestVerify(t *testing.T) {
	v, _ := newVerifier(t)

	tests := []struct {
		name   string
		key    *rsa.PrivateKey
		header func(map[string]any)
		claims func(map[string]any)
		ok     bool
	}{
		{name: "valid", ok: true},
		{name: "aud as one string", claims: func(c map[string]any) { c["aud"] = aud }, ok: true},
		{name: "within clock leeway", claims: func(c map[string]any) { c["exp"] = now.Add(-30 * time.Second).Unix() }, ok: true},
		{name: "signed by another key", key: other},
		{name: "wrong audience", claims: func(c map[string]any) { c["aud"] = []string{"someone-else"} }},
		{name: "wrong issuer", claims: func(c map[string]any) { c["iss"] = "https://evil.cloudflareaccess.com" }},
		{name: "expired", claims: func(c map[string]any) { c["exp"] = now.Add(-2 * time.Minute).Unix() }},
		{name: "no expiry", claims: func(c map[string]any) { delete(c, "exp") }},
		{name: "not valid yet", claims: func(c map[string]any) { c["nbf"] = now.Add(5 * time.Minute).Unix() }},
		{name: "no email", claims: func(c map[string]any) { delete(c, "email") }},
		{name: "alg none", header: func(h map[string]any) { h["alg"] = "none" }},
		{name: "alg HS256", header: func(h map[string]any) { h["alg"] = "HS256" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, header, claims := signer, goodHeader(), goodClaims(v.Issuer)
			if tt.key != nil {
				key = tt.key
			}
			if tt.header != nil {
				tt.header(header)
			}
			if tt.claims != nil {
				tt.claims(claims)
			}
			email, err := v.Verify(context.Background(), sign(t, key, header, claims))
			if tt.ok && (err != nil || email != "ryan@example.com") {
				t.Errorf("Verify = %q, %v; want ryan@example.com", email, err)
			}
			if !tt.ok && err == nil {
				t.Errorf("Verify accepted the token for %s", email)
			}
		})
	}
}

func TestVerifyMalformed(t *testing.T) {
	v, _ := newVerifier(t)
	good := sign(t, signer, goodHeader(), goodClaims(v.Issuer))
	parts := strings.Split(good, ".")

	for _, token := range []string{
		"",
		"not-a-token",
		parts[0] + "." + parts[1],          // no signature
		parts[0] + "." + parts[1] + ".!!!", // signature isn't base64
		"!!!." + parts[1] + "." + parts[2], // header isn't base64
		// Claims swapped after signing.
		parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"email":"x@example.com"}`)) + "." + parts[2],
	} {
		if _, err := v.Verify(context.Background(), token); err == nil {
			t.Errorf("Verify(%q) accepted a malformed token", token)
		}
	}
}

func TestKeyFetching(t *testing.T) {
	v, fetches := newVerifier(t)
	ctx := context.Background()
	good := sign(t, signer, goodHeader(), goodClaims(v.Issuer))

	for range 3 {
		if _, err := v.Verify(ctx, good); err != nil {
			t.Fatal(err)
		}
	}
	if n := fetches.Load(); n != 1 {
		t.Errorf("3 tokens with a known key fetched the keys %d times, want 1", n)
	}

	// An unknown key ID refetches, but at most once a minute.
	header := goodHeader()
	header["kid"] = "rotated"
	unknown := sign(t, signer, header, goodClaims(v.Issuer))
	clock := now
	v.Now = func() time.Time { return clock }

	v.Verify(ctx, unknown)
	if n := fetches.Load(); n != 1 {
		t.Errorf("an unknown key within a minute fetched again: %d fetches, want 1", n)
	}
	clock = now.Add(2 * time.Minute)
	v.Verify(ctx, unknown)
	v.Verify(ctx, unknown)
	if n := fetches.Load(); n != 2 {
		t.Errorf("an unknown key after a minute: %d fetches, want 2", n)
	}
}

func TestMiddleware(t *testing.T) {
	v, _ := newVerifier(t)
	h := v.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(Email(r.Context())))
	}))

	tests := []struct {
		name  string
		token string
		want  int
		body  string
	}{
		{"valid token", sign(t, signer, goodHeader(), goodClaims(v.Issuer)), http.StatusOK, "ryan@example.com"},
		{"no token", "", http.StatusForbidden, ""},
		{"bad token", sign(t, other, goodHeader(), goodClaims(v.Issuer)), http.StatusForbidden, ""},
	}
	for _, tt := range tests {
		req := httptest.NewRequest("GET", "/api/status", nil)
		if tt.token != "" {
			req.Header.Set(Header, tt.token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tt.want {
			t.Errorf("%s: status %d, want %d", tt.name, rec.Code, tt.want)
		}
		if tt.body != "" && rec.Body.String() != tt.body {
			t.Errorf("%s: handler saw email %q, want %q", tt.name, rec.Body.String(), tt.body)
		}
	}
}
