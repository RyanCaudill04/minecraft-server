// Package access verifies the Cloudflare Access token on every request, so
// the admin app doesn't rely on the login at the edge alone: anything that
// reaches the origin directly (a misconfigured route, another process on
// the host) has no valid token and gets 403.
//
// Access sends a signed JWT in the Cf-Access-Jwt-Assertion header. Its
// signing keys are published at <issuer>/cdn-cgi/access/certs and rotate,
// so they're cached by key ID and fetched again when an unknown ID shows
// up, at most once every 10 seconds. Only RS256 is accepted, which is what Access signs with; accepting
// anything else invites algorithm-confusion bugs.
package access

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Header is the request header Access puts the token in.
const Header = "Cf-Access-Jwt-Assertion"

const (
	leeway       = time.Minute      // clock skew allowed on exp and nbf
	refetchEvery = 10 * time.Second // most often an unknown key ID refetches the keys
	logEvery     = 10 * time.Second // most often a rejected token is logged
	maxKeySet    = 1 << 20          // largest key set response read
	minKeyBits   = 2048
)

// Verifier checks Access tokens for one Access application.
type Verifier struct {
	Issuer string // team URL, e.g. https://myteam.cloudflareaccess.com
	AUD    string // the application's audience tag
	HTTP   *http.Client
	Now    func() time.Time // replaced in tests

	mu   sync.Mutex                // guards keys; never held during a fetch
	keys map[string]*rsa.PublicKey // by key ID

	fetchMu sync.Mutex // one fetch at a time; guards tried
	tried   time.Time  // when the last fetch started

	logMu      sync.Mutex
	logged     time.Time // when a rejection was last logged
	suppressed int       // rejections not logged since then
}

// Verify checks token's signature, audience, issuer and lifetime, and
// returns the email of the user it was issued to.
func (v *Verifier) Verify(ctx context.Context, token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("malformed token")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodePart(parts[0], &header); err != nil {
		return "", fmt.Errorf("header: %w", err)
	}
	if header.Alg != "RS256" {
		return "", fmt.Errorf("algorithm %q not allowed", header.Alg)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", fmt.Errorf("signature: %w", err)
	}
	key, err := v.key(ctx, header.Kid)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return "", errors.New("bad signature")
	}

	// Claims are only trusted once the signature checks out.
	var claims struct {
		Aud   audience `json:"aud"`
		Iss   string   `json:"iss"`
		Exp   int64    `json:"exp"`
		Nbf   int64    `json:"nbf"`
		Email string   `json:"email"`
	}
	if err := decodePart(parts[1], &claims); err != nil {
		return "", fmt.Errorf("claims: %w", err)
	}
	now := v.now()
	switch {
	case v.AUD == "" || !slices.Contains(claims.Aud, v.AUD):
		return "", errors.New("wrong audience")
	case claims.Iss != v.Issuer:
		return "", fmt.Errorf("wrong issuer %q", claims.Iss)
	case claims.Exp == 0 || now.After(time.Unix(claims.Exp, 0).Add(leeway)):
		return "", errors.New("expired")
	case now.Add(leeway).Before(time.Unix(claims.Nbf, 0)):
		return "", errors.New("not valid yet")
	case claims.Email == "":
		// Service tokens carry no email; admin needs one for its audit log.
		return "", errors.New("no email")
	}
	return claims.Email, nil
}

// Middleware rejects requests without a valid token with 403, and passes
// the user's email on to next in the request's context (see Email).
func (v *Verifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get(Header)
		if token == "" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		email, err := v.Verify(r.Context(), token)
		if err != nil {
			v.logRejection(r, err)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithEmail(r.Context(), email)))
	})
}

// logRejection logs why a token was refused, at most once per logEvery,
// so junk tokens sent at request rate can't flood the journal. The
// rejections in between are counted in the next line.
func (v *Verifier) logRejection(r *http.Request, err error) {
	v.logMu.Lock()
	defer v.logMu.Unlock()
	now := v.now()
	if !v.logged.IsZero() && now.Sub(v.logged) < logEvery {
		v.suppressed++
		return
	}
	more := ""
	if v.suppressed > 0 {
		more = fmt.Sprintf(" (and %d more since the last log line)", v.suppressed)
	}
	log.Printf("access: refused %s %s: %q%s", r.Method, r.URL.Path, err.Error(), more)
	v.logged, v.suppressed = now, 0
}

type emailKey struct{}

// WithEmail returns ctx carrying the signed-in user's email.
func WithEmail(ctx context.Context, email string) context.Context {
	return context.WithValue(ctx, emailKey{}, email)
}

// Email returns the email Middleware verified for this request, or "".
func Email(ctx context.Context) string {
	email, _ := ctx.Value(emailKey{}).(string)
	return email
}

// key returns the public key with ID kid, fetching the key set if kid is
// new. Requests for a cached key never wait on a fetch. Fetch attempts,
// failed ones included, are limited to one per refetchEvery, so tokens with
// made-up key IDs can't make every request call Cloudflare. A failed fetch
// keeps the keys already cached.
func (v *Verifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	if k := v.cached(kid); k != nil {
		return k, nil
	}
	v.fetchMu.Lock()
	defer v.fetchMu.Unlock()
	// Another request may have fetched it while this one waited.
	if k := v.cached(kid); k != nil {
		return k, nil
	}
	if !v.tried.IsZero() && v.now().Sub(v.tried) < refetchEvery {
		return nil, fmt.Errorf("unknown key %q", kid)
	}
	v.tried = v.now()
	// The keys are shared, so one visitor leaving doesn't cancel the fetch.
	keys, err := v.fetchKeys(context.WithoutCancel(ctx))
	if err != nil {
		return nil, fmt.Errorf("fetching keys: %w", err)
	}
	v.mu.Lock()
	v.keys = keys
	v.mu.Unlock()
	if k := v.cached(kid); k != nil {
		return k, nil
	}
	return nil, fmt.Errorf("unknown key %q", kid)
}

func (v *Verifier) cached(kid string) *rsa.PublicKey {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.keys[kid]
}

func (v *Verifier) fetchKeys(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", v.Issuer+"/cdn-cgi/access/certs", nil)
	if err != nil {
		return nil, err
	}
	client := v.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var set struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxKeySet)).Decode(&set); err != nil {
		return nil, err
	}
	// One bad entry is skipped rather than failing the set, so it can't
	// lock out tokens signed with the good keys next to it.
	keys := make(map[string]*rsa.PublicKey)
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		key, err := rsaKey(k.N, k.E)
		if err != nil {
			log.Printf("access: skipping key %q: %v", k.Kid, err)
			continue
		}
		keys[k.Kid] = key
	}
	if len(keys) == 0 {
		return nil, errors.New("no usable RSA keys")
	}
	return keys, nil
}

// rsaKey decodes a JWK's base64url modulus and exponent.
func rsaKey(n64, e64 string) (*rsa.PublicKey, error) {
	nb, err := base64.RawURLEncoding.DecodeString(n64)
	if err != nil {
		return nil, fmt.Errorf("modulus: %w", err)
	}
	eb, err := base64.RawURLEncoding.DecodeString(e64)
	if err != nil {
		return nil, fmt.Errorf("exponent: %w", err)
	}
	n, e := new(big.Int).SetBytes(nb), new(big.Int).SetBytes(eb)
	if n.BitLen() < minKeyBits {
		return nil, fmt.Errorf("%d-bit modulus, want at least %d", n.BitLen(), minKeyBits)
	}
	if !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 || e.Bit(0) == 0 {
		return nil, errors.New("exponent isn't an odd number from 3 to 2^31-1")
	}
	return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func decodePart(s string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// audience is a JWT "aud" claim, which may be one string or a list.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = audience{one}
		return nil
	}
	return json.Unmarshal(b, (*[]string)(a))
}
