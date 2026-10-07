package localdns

import (
	"net"
	"testing"

	"github.com/miekg/dns"
)

func TestHandle(t *testing.T) {
	s, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	for _, network := range []string{"udp", "tcp"} {
		c := &dns.Client{Net: network}
		for _, name := range []string{"web-x.example.dev.", "anything.at.all.", "localhost."} {
			for _, tc := range []struct {
				qtype uint16
				want  net.IP
			}{
				{dns.TypeA, net.IPv4(127, 0, 0, 1)},
				{dns.TypeAAAA, net.IPv6loopback},
				{dns.TypeMX, nil},
				{dns.TypeTXT, nil},
			} {
				q := new(dns.Msg).SetQuestion(name, tc.qtype)
				r, _, err := c.Exchange(q, s.Addr())
				if err != nil {
					t.Fatalf("%s %s %s: %v", network, name, dns.TypeToString[tc.qtype], err)
				}
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

func TestListenAddressInUse(t *testing.T) {
	s, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := Listen(s.Addr()); err == nil {
		t.Fatalf("second Listen on %s succeeded", s.Addr())
	}
}
