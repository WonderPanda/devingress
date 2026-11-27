package remote

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/WonderPanda/devingress/internal/routefile"
)

// fakeToken matches caddy-dns/cloudflare's format check; it is no real token.
const fakeToken = "cfut_" + "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN"

const suffix = "machine.example.dev"

func TestName(t *testing.T) {
	opted := routefile.Owner{Dir: "/a", Domain: "example.dev", Remote: true}
	for name, c := range []struct {
		o          routefile.Owner
		host       string
		suffix     string
		twin, skip string
	}{
		{opted, "web-my-branch.example.dev", suffix, "web-my-branch.machine.example.dev", ""},
		{opted, "a.b.example.dev", suffix, "", `"a.b" is 2 DNS labels, and the wildcard certificate *.machine.example.dev covers one`},
		{opted, strings.Repeat("x", 64) + ".example.dev", suffix, "", "is 64 characters, and a DNS label holds at most 63"},
		{opted, strings.Repeat("x", 63) + ".example.dev", suffix, strings.Repeat("x", 63) + ".machine.example.dev", ""},
		{opted, "-web.example.dev", suffix, "", "is not a DNS label"},
		{routefile.Owner{Remote: true}, "web", suffix, "", "the route file sets no domain"},
		{routefile.Owner{Domain: "example.dev"}, "web.example.dev", suffix, "", ""}, // remote unset
		{opted, "web.example.dev", "", "", ""},                                      // remote mode off
	} {
		twin, skip := Name(c.o, c.host, c.suffix)
		if twin != c.twin || (c.skip == "") != (skip == "") || !strings.Contains(skip, c.skip) {
			t.Errorf("case %d: Name(%s) = %q, %q; want %q, %q", name, c.host, twin, skip, c.twin, c.skip)
		}
	}
}

func ips(s ...string) []net.IP {
	var out []net.IP
	for _, a := range s {
		out = append(out, net.ParseIP(a))
	}
	return out
}

func TestTailnetIPv4(t *testing.T) {
	lo := Iface{Name: "lo", Addrs: ips("127.0.0.1", "::1")}
	for _, c := range []struct {
		name   string
		goos   string
		ifaces []Iface
		want   string // "" expects an error containing errHas
		errHas string
	}{
		{"linux one", "linux", []Iface{lo, {Name: "tailscale0", Addrs: ips("100.64.0.10", "fd7a:115c:a1e0::a")}}, "100.64.0.10", ""},
		{"linux none", "linux", []Iface{lo, {Name: "eth0", Addrs: ips("100.64.0.10")}}, "", "no tailnet address (100.64.0.0/10) on interface tailscale0"},
		{"linux outside range", "linux", []Iface{{Name: "tailscale0", Addrs: ips("100.128.0.1")}}, "", "no tailnet address"},
		{"linux several", "linux", []Iface{{Name: "tailscale0", Addrs: ips("100.64.0.10", "100.100.1.2")}}, "", "several tailnet addresses (100.64.0.10 on tailscale0, 100.100.1.2 on tailscale0)"},
		{"darwin one", "darwin", []Iface{lo, {Name: "utun0", Addrs: ips("fe80::1")}, {Name: "utun4", Addrs: ips("100.64.0.10")}}, "100.64.0.10", ""},
		{"darwin none", "darwin", []Iface{lo, {Name: "tailscale0", Addrs: ips("100.64.0.10")}}, "", "on any utun interface"},
		{"darwin several", "darwin", []Iface{{Name: "utun3", Addrs: ips("100.64.0.10")}, {Name: "utun5", Addrs: ips("100.64.0.11")}}, "", "100.64.0.10 on utun3, 100.64.0.11 on utun5"},
	} {
		got, err := TailnetIPv4(c.ifaces, c.goos)
		switch {
		case c.want != "" && (err != nil || got.String() != c.want):
			t.Errorf("%s: got %v, %v; want %s", c.name, got, err, c.want)
		case c.want == "" && (err == nil || !strings.Contains(err.Error(), c.errHas)):
			t.Errorf("%s: got %v, %v; want an error containing %q", c.name, got, err, c.errHas)
		}
	}
}

func TestReadToken(t *testing.T) {
	dir := t.TempDir()
	for name, c := range map[string]struct {
		body   *string
		errHas string
	}{
		"newline":       {ptr(fakeToken + "\n"), ""},
		"crlf":          {ptr(fakeToken + "\r\n"), ""},
		"legacy":        {ptr(strings.Repeat("a", 40)), ""},
		"missing":       {nil, "does not exist"},
		"empty":         {ptr(""), "is empty"},
		"spaces only":   {ptr("  \n"), "is empty"},
		"trailing junk": {ptr(fakeToken + " \n"), "does not hold a Cloudflare API token"},
		"two lines":     {ptr(fakeToken + "\n" + fakeToken + "\n"), "does not hold a Cloudflare API token"},
		"too short":     {ptr("cfut_abc"), "does not hold a Cloudflare API token"},
	} {
		path := filepath.Join(dir, name)
		if c.body != nil {
			if err := os.WriteFile(path, []byte(*c.body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		tok, err := ReadToken(path)
		if c.errHas == "" {
			if err != nil || tok == "" || strings.ContainsAny(tok, "\r\n") {
				t.Errorf("%s: %q, %v", name, tok, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.errHas) || !strings.Contains(err.Error(), path) {
			t.Errorf("%s: err = %v, want one naming %s and containing %q", name, err, path, c.errHas)
		}
		if err != nil && strings.Contains(err.Error(), fakeToken) {
			t.Errorf("%s: error quotes the token: %v", name, err)
		}
	}
}

func ptr(s string) *string { return &s }

func TestTokenModeWarning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(path, []byte(fakeToken), 0o600); err != nil {
		t.Fatal(err)
	}
	if w := TokenModeWarning(path); w != "" {
		t.Errorf("mode 600: %q", w)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if w := TokenModeWarning(path); !strings.Contains(w, "mode 0640") {
		t.Errorf("mode 640: %q", w)
	}
}
