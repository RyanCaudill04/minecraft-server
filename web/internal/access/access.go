// Package access verifies the Cloudflare Access token on every request, so
// the admin app doesn't rely on the login at the edge alone: anything that
// reaches the origin directly (a misconfigured route, another process on
// the host) has no valid token and gets 403.
//
// Access sends a signed JWT in the Cf-Access-Jwt-Assertion header. Its
// signing keys are published at <issuer>/cdn-cgi/access/certs and rotate,
// so they're cached by key ID and fetched again when an unknown ID shows
// up. Only RS256 is accepted, which is what Access signs with; accepting
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
	leeway       = time.Minute // clock skew allowed on exp and nbf
	refetchEvery = time.Minute // most often an unknown key ID refetches the keys
)

// Verifier checks Access tokens for one Access application.
type Verifier struct {
	Issuer string // team URL, e.g. https://myteam.cloudflareaccess.com
	AUD    string // the application's audience tag
	HTTP   *http.Client
	Now    func() time.Time // replaced in tests

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey // by key ID
	fetched time.Time
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
			log.Printf("access: %s %s: %v", r.Method, r.URL.Path, err)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithEmail(r.Context(), email)))
	})
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
// new. Fetches are limited to one per refetchEvery, so tokens with made-up
// key IDs can't make every request call Cloudflare.
func (v *Verifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	if !v.fetched.IsZero() && v.now().Sub(v.fetched) < refetchEvery {
		return nil, fmt.Errorf("unknown key %q", kid)
	}
	v.fetched = v.now()
	keys, err := v.fetchKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetching keys: %w", err)
	}
	v.keys = keys
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown key %q", kid)
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
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return nil, err
	}
	keys := make(map[string]*rsa.PublicKey)
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(e) == 0 || len(e) > 4 {
			return nil, fmt.Errorf("key %q: bad n or e", k.Kid)
		}
		keys[k.Kid] = &rsa.PublicKey{
			N: new(big.Int).SetBytes(n),
			E: int(new(big.Int).SetBytes(e).Int64()),
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("no RSA keys")
	}
	return keys, nil
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
