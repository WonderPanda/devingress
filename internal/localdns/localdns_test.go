package localdns

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/miekg/dns"

	"github.com/WonderPanda/devingress/internal/macresolver"
	"github.com/WonderPanda/devingress/internal/resolved"
)

// listen starts a listener on a free port that reads its domains from resolved drop-ins in a
// temp dir, which it returns.
func listen(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Listen("127.0.0.1:0", func() ([]string, error) { return resolved.Domains(dir) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

// route writes the drop-in "devingress dns setup" writes for domain into dir.
func route(t *testing.T, dir, domain string) {
	t.Helper()
	conf := resolved.DropIn("127.0.0.1:15353", domain, false)
	if err := os.WriteFile(filepath.Join(dir, resolved.DropInName(domain)), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
}

func query(t *testing.T, network, addr, name string, qtype uint16) *dns.Msg {
	t.Helper()
	r, _, err := (&dns.Client{Net: network}).Exchange(new(dns.Msg).SetQuestion(name, qtype), addr)
	if err != nil {
		t.Fatalf("%s %s %s: %v", network, name, dns.TypeToString[qtype], err)
	}
	return r
}

func TestHandle(t *testing.T) {
	s, dir := listen(t)
	route(t, dir, "example.dev")

	for _, network := range []string{"udp", "tcp"} {
		for _, name := range []string{"web-x.example.dev.", "example.dev.", "Web-X.Example.DEV.", "a.b.example.dev."} {
			for _, tc := range []struct {
				qtype uint16
				want  net.IP
			}{
				{dns.TypeA, net.IPv4(127, 0, 0, 1)},
				{dns.TypeAAAA, net.IPv6loopback},
				{dns.TypeMX, nil},
				{dns.TypeTXT, nil},
			} {
				r := query(t, network, s.Addr(), name, tc.qtype)
				label := network + " " + name + " " + dns.TypeToString[tc.qtype]
				if r.Rcode != dns.RcodeSuccess || !r.Authoritative {
					t.Errorf("%s: rcode %s, authoritative %v", label, dns.RcodeToString[r.Rcode], r.Authoritative)
				}
				if tc.want == nil {
					if len(r.Answer) != 0 {
						t.Errorf("%s: answer %v, want none", label, r.Answer)
					}
					continue
				}
				if len(r.Answer) != 1 {
					t.Fatalf("%s: answer %v, want one record", label, r.Answer)
				}
				var got net.IP
				switch rr := r.Answer[0].(type) {
				case *dns.A:
					got = rr.A
				case *dns.AAAA:
					got = rr.AAAA
				}
				if hdr := r.Answer[0].Header(); !got.Equal(tc.want) || hdr.Name != name || hdr.Ttl != TTL {
					t.Errorf("%s: got %v", label, r.Answer[0])
				}
			}
		}
	}
}

// assertRefused checks that name gets REFUSED with no records, for A and AAAA over UDP and TCP.
func assertRefused(t *testing.T, addr, name string) {
	t.Helper()
	for _, network := range []string{"udp", "tcp"} {
		for _, qtype := range []uint16{dns.TypeA, dns.TypeAAAA} {
			r := query(t, network, addr, name, qtype)
			if r.Rcode != dns.RcodeRefused || len(r.Answer)+len(r.Ns)+len(r.Extra) != 0 {
				t.Errorf("%s %s %s: rcode %s, answer %v, want REFUSED and no records",
					network, name, dns.TypeToString[qtype], dns.RcodeToString[r.Rcode], r.Answer)
			}
		}
	}
}

func TestRefusesUnroutedNames(t *testing.T) {
	s, dir := listen(t)
	route(t, dir, "eluve.dev")
	for _, name := range []string{"api.github.com.", "github.com.", "noteluve.dev.", "web.noteluve.dev.", "dev.", "localhost."} {
		assertRefused(t, s.Addr(), name)
	}

	// A devingress drop-in that does not route its own domain, and a drop-in devingress did not
	// write, route nothing to the listener.
	for name, conf := range map[string]string{
		resolved.DropInName("other.dev"): "[Resolve]\nDNS=127.0.0.1:15353\n",
		"third-dev.conf":                 resolved.DropIn("127.0.0.1:15353", "third.dev", false),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(conf), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	assertRefused(t, s.Addr(), "web.other.dev.")
	assertRefused(t, s.Addr(), "web.third.dev.")
}

func TestEmptyListRefusesAll(t *testing.T) {
	s, _ := listen(t)
	for _, name := range []string{"web.example.dev.", "api.github.com.", "localhost."} {
		assertRefused(t, s.Addr(), name)
	}

	missing := filepath.Join(t.TempDir(), "gone")
	for label, domains := range map[string]func() ([]string, error){
		"missing dir": func() ([]string, error) { return resolved.Domains(missing) },
		"read error":  func() ([]string, error) { return []string{"example.dev"}, os.ErrPermission },
	} {
		s, err := Listen("127.0.0.1:0", domains)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(label)
		assertRefused(t, s.Addr(), "web.example.dev.")
		s.Close()
	}
}

// TestDomainAddedAfterStart routes a domain while the listener runs: the next query answers.
func TestDomainAddedAfterStart(t *testing.T) {
	s, dir := listen(t)
	assertRefused(t, s.Addr(), "web.later.dev.")
	route(t, dir, "later.dev")
	for _, qtype := range []uint16{dns.TypeA, dns.TypeAAAA} {
		r := query(t, "udp", s.Addr(), "web.later.dev.", qtype)
		if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 {
			t.Errorf("after setup, %s: rcode %s, answer %v", dns.TypeToString[qtype], dns.RcodeToString[r.Rcode], r.Answer)
		}
	}
}

// TestMacResolverDomains reads the domain list from /etc/resolver-style files: only the ones
// devingress wrote route.
func TestMacResolverDomains(t *testing.T) {
	dir := t.TempDir()
	ours, err := macresolver.File("127.0.0.1:15353", "example.dev")
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"example.dev": ours, "corp.dev": "nameserver 10.0.0.1\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Listen("127.0.0.1:0", func() ([]string, error) { return macresolver.Domains(dir) })
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if r := query(t, "udp", s.Addr(), "web.example.dev.", dns.TypeA); r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 {
		t.Errorf("web.example.dev: rcode %s, answer %v", dns.RcodeToString[r.Rcode], r.Answer)
	}
	assertRefused(t, s.Addr(), "web.corp.dev.")
}

func TestListenAddressInUse(t *testing.T) {
	s, _ := listen(t)
	if _, err := Listen(s.Addr(), func() ([]string, error) { return nil, nil }); err == nil {
		t.Fatalf("second Listen on %s succeeded", s.Addr())
	}
}
