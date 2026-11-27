package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeCloudflare is the slice of Cloudflare's v4 API libdns's provider uses: list zones, look
// a record up by type and name, create one, patch one. It records every write.
type fakeCloudflare struct {
	t       *testing.T
	mu      sync.Mutex
	zones   map[string]string // id -> name
	records []fakeRecord
	writes  []string // "POST <name> <content>" or "PATCH <id> <content>"
	calls   int
	auth    map[string]bool
	fail    int // when non-zero, every request answers this status
	nextID  int
}

type fakeRecord struct {
	ID      string `json:"id"`
	ZoneID  string `json:"zone_id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl"`
}

func newFake(t *testing.T) (*fakeCloudflare, *rewriter) {
	f := &fakeCloudflare{t: t, zones: map[string]string{"z1": "example.dev", "z2": "other.dev"}, auth: map[string]bool{}}
	f.records = []fakeRecord{
		{ID: "r-www", ZoneID: "z1", Type: "A", Name: "www.example.dev", Content: "203.0.113.5", TTL: 1},
		{ID: "r-other", ZoneID: "z1", Type: "A", Name: "*.other-machine.example.dev", Content: "100.64.0.99", TTL: 1},
		{ID: "r-txt", ZoneID: "z1", Type: "TXT", Name: "machine.example.dev", Content: `"keep me"`, TTL: 1},
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)
	return f, &rewriter{base: base}
}

// rewriter sends libdns's requests for https://api.cloudflare.com to the fake instead.
type rewriter struct{ base *url.URL }

func (r *rewriter) Do(req *http.Request) (*http.Response, error) {
	if req.URL.Host != "api.cloudflare.com" {
		return nil, fmt.Errorf("request to %s, not api.cloudflare.com", req.URL.Host)
	}
	req.URL.Scheme, req.URL.Host = r.base.Scheme, r.base.Host
	return http.DefaultClient.Do(req)
}

func (f *fakeCloudflare) reply(w http.ResponseWriter, status int, result any) {
	n := 1
	if v, ok := result.([]fakeRecord); ok {
		n = len(v)
	}
	if v, ok := result.([]map[string]string); ok {
		n = len(v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"success": status < 400, "result": result, "errors": []any{},
		"result_info": map[string]int{"page": 1, "per_page": 100, "count": n, "total_count": n},
	})
}

func (f *fakeCloudflare) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.auth[r.Header.Get("Authorization")] = true
	if f.fail != 0 {
		w.WriteHeader(f.fail)
		json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]any{{"code": 10000, "message": "Authentication error"}}})
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/client/v4/"), "/")
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && len(parts) == 1 && parts[0] == "zones":
		var zones []map[string]string
		for id, name := range f.zones {
			if q.Get("name") == "" || q.Get("name") == name {
				zones = append(zones, map[string]string{"id": id, "name": name})
			}
		}
		f.reply(w, 200, zones)
	case r.Method == http.MethodGet && len(parts) == 3 && parts[2] == "dns_records":
		recs := []fakeRecord{}
		for _, rec := range f.records {
			if rec.ZoneID == parts[1] && (q.Get("type") == "" || q.Get("type") == rec.Type) && (q.Get("name") == "" || q.Get("name") == rec.Name) {
				recs = append(recs, rec)
			}
		}
		f.reply(w, 200, recs)
	case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "dns_records":
		var rec fakeRecord
		json.NewDecoder(r.Body).Decode(&rec)
		if zone := f.zones[parts[1]]; !strings.HasSuffix(rec.Name, "."+zone) {
			rec.Name += "." + zone // the API takes a name relative to the zone
		}
		f.nextID++
		rec.ID, rec.ZoneID = fmt.Sprintf("new-%d", f.nextID), parts[1]
		f.records = append(f.records, rec)
		f.writes = append(f.writes, "POST "+rec.Name+" "+rec.Content)
		f.reply(w, 200, rec)
	case r.Method == http.MethodPatch && len(parts) == 4 && parts[2] == "dns_records":
		var patch fakeRecord
		json.NewDecoder(r.Body).Decode(&patch)
		i := slices.IndexFunc(f.records, func(rec fakeRecord) bool { return rec.ID == parts[3] })
		if i < 0 {
			f.reply(w, 404, nil)
			return
		}
		f.records[i].Content = patch.Content
		f.writes = append(f.writes, "PATCH "+parts[3]+" "+patch.Content)
		f.reply(w, 200, f.records[i])
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL)
		f.reply(w, 404, nil)
	}
}

func (f *fakeCloudflare) snapshot() ([]fakeRecord, []string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.records), slices.Clone(f.writes), f.calls
}

type testUpdater struct {
	*Updater
	ip   string
	logs []string
}

func newUpdater(t *testing.T, client *rewriter) *testUpdater {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "cloudflare-token")
	if err := os.WriteFile(tokenFile, []byte(fakeToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tu := &testUpdater{ip: "100.64.0.10"}
	tu.Updater = &Updater{
		Suffix:    suffix,
		TokenFile: tokenFile,
		GOOS:      "linux",
		Interfaces: func() ([]Iface, error) {
			if tu.ip == "" {
				return []Iface{{Name: "lo", Addrs: ips("127.0.0.1")}}, nil
			}
			return []Iface{{Name: "tailscale0", Addrs: ips(tu.ip)}}, nil
		},
		HTTPClient: client,
		Logf:       func(format string, args ...any) { tu.logs = append(tu.logs, fmt.Sprintf(format, args...)) },
	}
	return tu
}

func TestUpdaterCreatesUpdatesAndSkips(t *testing.T) {
	f, client := newFake(t)
	before, _, _ := f.snapshot()
	u := newUpdater(t, client)
	ctx := context.Background()

	u.Tick(ctx) // no record yet: create it
	recs, writes, calls := f.snapshot()
	if want := []string{"POST *.machine.example.dev 100.64.0.10"}; !slices.Equal(writes, want) {
		t.Fatalf("writes = %q, want %q", writes, want)
	}
	created := recs[len(recs)-1]
	if created.Type != "A" || created.Proxied {
		t.Errorf("created %+v, want a DNS-only A record", created)
	}

	u.Tick(ctx) // same address: no request at all
	if _, _, again := f.snapshot(); again != calls {
		t.Errorf("an unchanged address made %d API requests", again-calls)
	}

	u.ip = "100.64.0.11"
	u.Tick(ctx) // new address: patch that record only
	recs, writes, _ = f.snapshot()
	if want := "PATCH " + created.ID + " 100.64.0.11"; len(writes) != 2 || writes[1] != want {
		t.Errorf("writes = %q, want the second to be %q", writes, want)
	}
	for _, b := range before {
		if i := slices.IndexFunc(recs, func(r fakeRecord) bool { return r.ID == b.ID }); i < 0 || recs[i] != b {
			t.Errorf("record %s changed: %+v", b.ID, recs[i])
		}
	}

	// A fresh serve finds the record pointing elsewhere and updates it in place.
	u2 := newUpdater(t, client)
	u2.ip = "100.64.0.12"
	u2.Tick(ctx)
	if _, writes, _ = f.snapshot(); writes[len(writes)-1] != "PATCH "+created.ID+" 100.64.0.12" {
		t.Errorf("writes = %q, want an update of %s", writes, created.ID)
	}
	if !f.auth["Bearer "+fakeToken] || len(f.auth) != 1 {
		t.Errorf("Authorization headers = %v, want the token's only", f.auth)
	}
	for _, l := range append(u.logs, u2.logs...) {
		if strings.Contains(l, fakeToken) {
			t.Errorf("log quotes the token: %q", l)
		}
	}
	if want := "*.machine.example.dev points at 100.64.0.11 (DNS-only)"; !slices.Contains(u.logs, want) {
		t.Errorf("logs = %q, want %q", u.logs, want)
	}
}

func TestUpdaterFailuresLogOnceAndRetry(t *testing.T) {
	f, client := newFake(t)
	u := newUpdater(t, client)
	ctx := context.Background()

	u.ip = ""
	u.Tick(ctx)
	u.Tick(ctx)
	if len(u.logs) != 1 || !strings.Contains(u.logs[0], "no tailnet address") {
		t.Fatalf("logs = %q, want one line about the missing address", u.logs)
	}
	if _, _, calls := f.snapshot(); calls != 0 {
		t.Errorf("%d API requests without an address", calls)
	}

	u.ip = "100.64.0.10"
	f.fail = http.StatusForbidden
	u.Tick(ctx)
	u.Tick(ctx)
	if len(u.logs) != 2 || !strings.Contains(u.logs[1], "HTTP 403") || !strings.Contains(u.logs[1], "retrying in 3m0s") {
		t.Fatalf("logs = %q, want one more line for the API failure", u.logs)
	}

	f.mu.Lock()
	f.fail = 0
	f.mu.Unlock()
	u.Tick(ctx) // the next tick retries and succeeds
	if _, writes, _ := f.snapshot(); len(writes) != 1 {
		t.Errorf("writes = %q, want the retry's create", writes)
	}
	for _, l := range u.logs {
		if strings.Contains(l, fakeToken) {
			t.Errorf("log quotes the token: %q", l)
		}
	}
}

func TestSetWildcardNeedsAZone(t *testing.T) {
	f, client := newFake(t)
	f.zones = map[string]string{"z2": "other.dev"}
	u := newUpdater(t, client)
	u.Tick(context.Background())
	if len(u.logs) != 1 || !strings.Contains(u.logs[0], "no Cloudflare zone the token can read holds machine.example.dev") {
		t.Errorf("logs = %q", u.logs)
	}
}
