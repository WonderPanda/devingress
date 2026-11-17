package resolved

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// testdata/resolvectl-dns-global.txt is "resolvectl dns" captured on a machine whose resolved
// has one global server, trimmed to a few links. resolvectl-dns-empty.txt is the same capture with
// the global server removed: resolvectl prints a scope without servers as a bare "<scope>:", as
// the links without servers show.
func TestGlobalServers(t *testing.T) {
	for file, want := range map[string][]string{
		"testdata/resolvectl-dns-global.txt": {"127.0.0.1:5354"},
		"testdata/resolvectl-dns-empty.txt":  nil,
	} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		got, err := GlobalServers(string(data))
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%s: got %q, %v; want %q", file, got, err, want)
		}
	}
	got, err := GlobalServers("Global: 1.1.1.1#cloudflare-dns.com 1.0.0.1#cloudflare-dns.com\nLink 2 (eth0): 10.0.0.1\n")
	if want := []string{"1.1.1.1#cloudflare-dns.com", "1.0.0.1#cloudflare-dns.com"}; err != nil || !slices.Equal(got, want) {
		t.Errorf("two servers: got %q, %v", got, err)
	}
	if _, err := GlobalServers("Link 2 (eth0): 10.0.0.1\n"); err == nil {
		t.Error("no Global line: want an error")
	}
}

func TestRoutesDomain(t *testing.T) {
	conf := "[Resolve]\n# Domains=~commented.dev\nDNS=127.0.0.1:5354\nDomains=~eluve.dev ~other.dev lan\n"
	for domain, want := range map[string]bool{
		"eluve.dev": true, "other.dev": true, "lan": false, "commented.dev": false, "dev": false,
	} {
		if got := RoutesDomain(conf, domain); got != want {
			t.Errorf("%s: got %v, want %v", domain, got, want)
		}
	}
}

func TestDropIn(t *testing.T) {
	if got, want := DropIn("127.0.0.1:15353", "example.dev", false),
		"[Resolve]\nDNS=127.0.0.1:15353\nDomains=~example.dev\n"; got != want {
		t.Errorf("plain:\n%s\nwant:\n%s", got, want)
	}
	want := `[Resolve]
# Cleared: global DNS servers are asked before devingress for ~example.dev and
# answer NXDOMAIN for a real domain. Other lookups use per-link DNS.
DNS=
DNS=127.0.0.1:15353
Domains=~example.dev
`
	if got := DropIn("127.0.0.1:15353", "example.dev", true); got != want {
		t.Errorf("reset:\n%s\nwant:\n%s", got, want)
	}
}

func TestDomain(t *testing.T) {
	for arg, want := range map[string]string{"example.dev": "example.dev", "Example.DEV.": "example.dev", "test": "test"} {
		if got, err := Domain(arg); err != nil || got != want {
			t.Errorf("%q: got %q, %v", arg, got, err)
		}
	}
	for _, arg := range []string{"", "~example.dev", "../etc", "a/b", "-x.dev", "a..dev", "*.dev"} {
		if got, err := Domain(arg); err == nil {
			t.Errorf("%q: got %q, want an error", arg, got)
		}
	}
}

func TestDomains(t *testing.T) {
	for name, want := range map[string]string{
		DropInName("example.dev"): "example.dev", "devingress-.conf": "", "eluve-dev.conf": "", "devingress-x.dev": "",
	} {
		if got, ok := DropInDomain(name); got != want || ok != (want != "") {
			t.Errorf("DropInDomain(%q) = %q, %v; want %q", name, got, ok, want)
		}
	}

	dir := t.TempDir()
	for name, conf := range map[string]string{
		DropInName("example.dev"): DropIn("127.0.0.1:15353", "example.dev", true),
		DropInName("stale.dev"):   "[Resolve]\nDNS=127.0.0.1:15353\n",
		"eluve-dev.conf":          "[Resolve]\nDNS=127.0.0.1:5354\nDomains=~eluve.dev\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(conf), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Domains(dir)
	if err != nil || !slices.Equal(got, []string{"example.dev"}) {
		t.Errorf("Domains = %q, %v; want [example.dev]", got, err)
	}
	if got, err := Domains(filepath.Join(dir, "missing")); err != nil || len(got) != 0 {
		t.Errorf("missing dir: %q, %v", got, err)
	}
}
