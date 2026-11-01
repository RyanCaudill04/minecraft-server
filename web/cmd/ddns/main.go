// Command ddns keeps the Minecraft address's DNS record pointing at the
// home connection's public IP. ddns.timer runs it every few minutes: it
// looks up the current public IPv4 address and, if the A record in
// Cloudflare says something else, updates it through the Cloudflare API.
//
// It reads its settings from the environment, which ddns.service loads
// from ~/.config/minecraft/secrets.env:
//
//	CLOUDFLARE_DNS_TOKEN  API token allowed to edit DNS on this zone only
//	CLOUDFLARE_ZONE_ID    the zone's ID, from the zone's overview page
//	DDNS_RECORD           the A record to keep updated, e.g. mc.example.com
//
// With none of them set it does nothing and exits cleanly, so the timer is
// harmless on a host that doesn't need dynamic DNS. The A record must
// already exist; ddns only changes its address.
//
// Usage: ddns [-dry-run] [-api url] [-ip-url url] [-timeout duration]
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"
)

// Environment variables ddns reads its settings from.
const (
	envToken  = "CLOUDFLARE_DNS_TOKEN"
	envZone   = "CLOUDFLARE_ZONE_ID"
	envRecord = "DDNS_RECORD"
)

// Config is what an update needs. Token is never logged.
type Config struct {
	Token  string
	ZoneID string
	Record string
}

// errNotConfigured means none of the settings are set.
var errNotConfigured = errors.New("not configured")

// configFromEnv reads the settings with getenv. It returns
// errNotConfigured if none are set, and names the missing ones if only
// some are.
func configFromEnv(getenv func(string) string) (Config, error) {
	c := Config{
		Token:  strings.TrimSpace(getenv(envToken)),
		ZoneID: strings.TrimSpace(getenv(envZone)),
		Record: strings.TrimSpace(getenv(envRecord)),
	}
	var missing []string
	for _, kv := range []struct{ key, val string }{{envToken, c.Token}, {envZone, c.ZoneID}, {envRecord, c.Record}} {
		if kv.val == "" {
			missing = append(missing, kv.key)
		}
	}
	switch len(missing) {
	case 0:
		return c, nil
	case 3:
		return c, errNotConfigured
	default:
		return c, fmt.Errorf("missing %s", strings.Join(missing, ", "))
	}
}

// Updater checks the public IP and updates the record.
type Updater struct {
	Config
	API    string // Cloudflare API base URL, ending in /client/v4
	IPURL  string // returns the caller's public IP, as "ip=..." lines or the bare address
	HTTP   *http.Client
	DryRun bool // report a change without making it
	Log    *log.Logger
}

// Update makes the record point at the current public IP. It reports
// whether the record changed (or, with DryRun, would have).
func (u *Updater) Update(ctx context.Context) (bool, error) {
	ip, err := u.publicIP(ctx)
	if err != nil {
		return false, err
	}
	rec, err := u.findRecord(ctx)
	if err != nil {
		return false, err
	}
	if rec.Content == ip.String() {
		u.Log.Printf("%s already points at %s", u.Record, ip)
		return false, nil
	}
	if u.DryRun {
		u.Log.Printf("would update %s from %s to %s (dry run)", u.Record, rec.Content, ip)
		return true, nil
	}
	if err := u.setRecord(ctx, rec.ID, ip); err != nil {
		return false, err
	}
	u.Log.Printf("updated %s from %s to %s", u.Record, rec.Content, ip)
	return true, nil
}

// publicIP asks IPURL for this connection's public IPv4 address.
func (u *Updater) publicIP(ctx context.Context) (netip.Addr, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.IPURL, nil)
	if err != nil {
		return netip.Addr{}, err
	}
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("public IP: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("public IP: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return netip.Addr{}, fmt.Errorf("public IP: %s answered %s", u.IPURL, resp.Status)
	}
	text := strings.TrimSpace(string(body))
	// Cloudflare's /cdn-cgi/trace answers with key=value lines.
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "ip="); ok {
			text = strings.TrimSpace(v)
			break
		}
	}
	ip, err := netip.ParseAddr(text)
	if err != nil || !ip.Is4() {
		return netip.Addr{}, fmt.Errorf("public IP: %s did not return an IPv4 address", u.IPURL)
	}
	return ip, nil
}

// record is the part of a Cloudflare DNS record ddns uses.
type record struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
}

// findRecord looks up the A record named Record. Exactly one must exist.
func (u *Updater) findRecord(ctx context.Context) (record, error) {
	q := url.Values{"type": {"A"}, "name": {u.Record}}
	var recs []record
	if err := u.call(ctx, http.MethodGet, u.recordsURL()+"?"+q.Encode(), nil, &recs); err != nil {
		return record{}, err
	}
	switch len(recs) {
	case 0:
		return record{}, fmt.Errorf("no A record named %s in the zone; create it once in the Cloudflare dashboard", u.Record)
	case 1:
		return recs[0], nil
	default:
		return record{}, fmt.Errorf("%d A records are named %s; keep only one", len(recs), u.Record)
	}
}

// setRecord changes only the record's address, keeping its TTL, proxy
// setting and comment.
func (u *Updater) setRecord(ctx context.Context, id string, ip netip.Addr) error {
	body, err := json.Marshal(map[string]string{"content": ip.String()})
	if err != nil {
		return err
	}
	var rec record
	return u.call(ctx, http.MethodPatch, u.recordsURL()+"/"+url.PathEscape(id), body, &rec)
}

func (u *Updater) recordsURL() string {
	return strings.TrimSuffix(u.API, "/") + "/zones/" + url.PathEscape(u.ZoneID) + "/dns_records"
}

// apiResponse is Cloudflare's envelope around every result.
type apiResponse struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Result json.RawMessage `json:"result"`
}

// call sends a Cloudflare API request and decodes its result into out.
func (u *Updater) call(ctx context.Context, method, endpoint string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+u.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("cloudflare: %w", err)
	}
	defer resp.Body.Close()
	var r apiResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return fmt.Errorf("cloudflare: %s %s: %s, unreadable body", method, req.URL.Path, resp.Status)
	}
	if !r.Success || resp.StatusCode != http.StatusOK {
		msgs := make([]string, 0, len(r.Errors))
		for _, e := range r.Errors {
			msgs = append(msgs, fmt.Sprintf("%d %s", e.Code, e.Message))
		}
		return fmt.Errorf("cloudflare: %s %s: %s: %s", method, req.URL.Path, resp.Status, strings.Join(msgs, "; "))
	}
	if err := json.Unmarshal(r.Result, out); err != nil {
		return fmt.Errorf("cloudflare: %s %s: %w", method, req.URL.Path, err)
	}
	return nil
}

func main() {
	api := flag.String("api", "https://api.cloudflare.com/client/v4", "Cloudflare API base URL")
	ipURL := flag.String("ip-url", "https://1.1.1.1/cdn-cgi/trace", "URL that returns this connection's public IPv4 address")
	dryRun := flag.Bool("dry-run", false, "report a change without making it")
	timeout := flag.Duration("timeout", 30*time.Second, "how long the whole update may take")
	flag.Parse()
	logger := log.New(os.Stderr, "ddns: ", 0) // journald adds timestamps

	cfg, err := configFromEnv(os.Getenv)
	if errors.Is(err, errNotConfigured) {
		logger.Printf("not configured, nothing to do: set %s, %s and %s in ~/.config/minecraft/secrets.env to enable it",
			envToken, envZone, envRecord)
		return
	}
	if err != nil {
		logger.Fatalf("%v in ~/.config/minecraft/secrets.env", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	u := &Updater{
		Config: cfg,
		API:    *api,
		IPURL:  *ipURL,
		HTTP:   &http.Client{Timeout: 10 * time.Second},
		DryRun: *dryRun,
		Log:    logger,
	}
	if _, err := u.Update(ctx); err != nil {
		logger.Fatal(err)
	}
}
