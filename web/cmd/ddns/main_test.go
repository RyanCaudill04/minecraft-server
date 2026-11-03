package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const (
	testToken = "test-token"
	testZone  = "zone123"
	testName  = "mc.example.test"
)

// fakeCloudflare is a fake Cloudflare DNS API holding one zone's records.
type fakeCloudflare struct {
	mu      sync.Mutex
	records []record
	patches int    // PATCH requests received
	fail    string // if set, every request fails with this message
}

func (f *fakeCloudflare) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(status int, result any, errMsg string) {
		res, _ := json.Marshal(result)
		resp := map[string]any{"success": errMsg == "", "errors": []any{}, "result": json.RawMessage(res)}
		if errMsg != "" {
			resp["errors"] = []any{map[string]any{"code": 1000, "message": errMsg}}
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(resp)
	}
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		reply(http.StatusForbidden, nil, "Authentication error")
		return
	}
	if f.fail != "" {
		reply(http.StatusInternalServerError, nil, f.fail)
		return
	}
	prefix := "/client/v4/zones/" + testZone + "/dns_records"
	switch {
	case r.Method == http.MethodGet && r.URL.Path == prefix:
		var out []record
		for _, rec := range f.records {
			if rec.Type == r.URL.Query().Get("type") && rec.Name == r.URL.Query().Get("name") {
				out = append(out, rec)
			}
		}
		reply(http.StatusOK, out, "")
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, prefix+"/"):
		f.patches++
		id := strings.TrimPrefix(r.URL.Path, prefix+"/")
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 1 || body["content"] == nil {
			reply(http.StatusBadRequest, nil, "bad body")
			return
		}
		for i := range f.records {
			if f.records[i].ID == id {
				f.records[i].Content = body["content"].(string)
				reply(http.StatusOK, f.records[i], "")
				return
			}
		}
		reply(http.StatusNotFound, nil, "Record does not exist")
	default:
		reply(http.StatusNotFound, nil, "no route")
	}
}

// newTest starts a fake public-IP service answering ipBody and a fake
// Cloudflare API, and returns an Updater using both.
func newTest(t *testing.T, ipBody string, cf *fakeCloudflare) *Updater {
	t.Helper()
	ipSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, ipBody)
	}))
	t.Cleanup(ipSrv.Close)
	cfSrv := httptest.NewServer(cf)
	t.Cleanup(cfSrv.Close)
	return &Updater{
		Config: Config{Token: testToken, ZoneID: testZone, Record: testName},
		API:    cfSrv.URL + "/client/v4",
		IPURL:  ipSrv.URL,
		HTTP:   cfSrv.Client(),
		Log:    log.New(io.Discard, "", 0),
	}
}

const trace = "fl=1\nh=1.1.1.1\nip=203.0.113.7\nts=1\nvisit_scheme=https\n"

func aRecord(content string) record {
	return record{ID: "rec1", Name: testName, Type: "A", Content: content}
}

func TestUpdateChangesRecord(t *testing.T) {
	cf := &fakeCloudflare{records: []record{aRecord("198.51.100.1"), {ID: "other", Name: "www.example.test", Type: "A", Content: "198.51.100.9"}}}
	u := newTest(t, trace, cf)
	changed, err := u.Update(context.Background())
	if err != nil || !changed {
		t.Fatalf("Update = %v, %v; want true, nil", changed, err)
	}
	if got := cf.records[0].Content; got != "203.0.113.7" {
		t.Errorf("record content = %q, want 203.0.113.7", got)
	}
	if got := cf.records[1].Content; got != "198.51.100.9" {
		t.Errorf("other record changed to %q", got)
	}
}

func TestUpdateUnchanged(t *testing.T) {
	cf := &fakeCloudflare{records: []record{aRecord("203.0.113.7")}}
	u := newTest(t, trace, cf)
	changed, err := u.Update(context.Background())
	if err != nil || changed {
		t.Fatalf("Update = %v, %v; want false, nil", changed, err)
	}
	if cf.patches != 0 {
		t.Errorf("sent %d PATCH requests, want 0", cf.patches)
	}
}

func TestUpdateDryRun(t *testing.T) {
	cf := &fakeCloudflare{records: []record{aRecord("198.51.100.1")}}
	u := newTest(t, trace, cf)
	u.DryRun = true
	changed, err := u.Update(context.Background())
	if err != nil || !changed {
		t.Fatalf("Update = %v, %v; want true, nil", changed, err)
	}
	if cf.patches != 0 || cf.records[0].Content != "198.51.100.1" {
		t.Errorf("dry run changed the record: %d patches, content %q", cf.patches, cf.records[0].Content)
	}
}

func TestUpdateBareIP(t *testing.T) {
	cf := &fakeCloudflare{records: []record{aRecord("198.51.100.1")}}
	u := newTest(t, " 203.0.113.8\n", cf)
	if _, err := u.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := cf.records[0].Content; got != "203.0.113.8" {
		t.Errorf("record content = %q, want 203.0.113.8", got)
	}
}

func TestUpdateErrors(t *testing.T) {
	tests := []struct {
		name    string
		ip      string
		cf      *fakeCloudflare
		token   string
		wantErr string
	}{
		{"no record", trace, &fakeCloudflare{}, testToken, "no A record named " + testName},
		{"two records", trace, &fakeCloudflare{records: []record{aRecord("1.2.3.4"), {ID: "rec2", Name: testName, Type: "A"}}}, testToken, "2 A records"},
		{"bad token", trace, &fakeCloudflare{records: []record{aRecord("1.2.3.4")}}, "wrong", "Authentication error"},
		{"api error", trace, &fakeCloudflare{fail: "internal"}, testToken, "1000 internal"},
		{"ipv6", "ip=2001:db8::1\n", &fakeCloudflare{records: []record{aRecord("1.2.3.4")}}, testToken, "did not return an IPv4 address"},
		{"garbage ip", "<html>", &fakeCloudflare{records: []record{aRecord("1.2.3.4")}}, testToken, "did not return an IPv4 address"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := newTest(t, tt.ip, tt.cf)
			u.Token = tt.token
			_, err := u.Update(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Update error = %v, want it to contain %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), tt.token) {
				t.Errorf("error %q contains the token", err)
			}
			if tt.cf.patches != 0 {
				t.Errorf("sent %d PATCH requests, want 0", tt.cf.patches)
			}
		})
	}
}

func TestUpdateIPServiceDown(t *testing.T) {
	cf := &fakeCloudflare{records: []record{aRecord("1.2.3.4")}}
	u := newTest(t, trace, cf)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	defer down.Close()
	u.IPURL = down.URL
	if _, err := u.Update(context.Background()); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("Update error = %v, want a 502", err)
	}
}

func TestConfigFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	full := map[string]string{envToken: "t", envZone: "z", envRecord: " mc.example.test "}
	c, err := configFromEnv(env(full))
	if err != nil || c != (Config{Token: "t", ZoneID: "z", Record: "mc.example.test"}) {
		t.Errorf("full config = %+v, %v", c, err)
	}
	if _, err := configFromEnv(env(nil)); !errors.Is(err, errNotConfigured) {
		t.Errorf("empty config error = %v, want errNotConfigured", err)
	}
	_, err = configFromEnv(env(map[string]string{envToken: "t"}))
	if err == nil || errors.Is(err, errNotConfigured) || !strings.Contains(err.Error(), envZone+", "+envRecord) {
		t.Errorf("partial config error = %v, want it to name %s and %s", err, envZone, envRecord)
	}
}
