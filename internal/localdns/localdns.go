// Package localdns is serve's DNS listener: every name under a domain "devingress dns setup"
// routed to it is loopback, and every other name is REFUSED. systemd-resolved puts a drop-in's
// DNS= in the global scope, which also serves as the default route when no link has DNS, so the
// listener can be asked for any name, and a loopback answer for api.github.com would be cached.
package localdns

import (
	"errors"
	"fmt"
	"net"
	"runtime"
	"strings"

	"github.com/miekg/dns"

	"github.com/WonderPanda/devingress/internal/macresolver"
	"github.com/WonderPanda/devingress/internal/resolved"
)

// TTL is the lifetime of every answer, in seconds.
const TTL = 5

// Routed lists the domains "devingress dns setup" routed to serve on this machine: the
// devingress drop-ins in resolved.Dir on Linux, the devingress files in macresolver.Dir on macOS.
func Routed() ([]string, error) {
	if runtime.GOOS == "darwin" {
		return macresolver.Domains(macresolver.Dir)
	}
	return resolved.Domains(resolved.Dir)
}

// under reports whether name, a query name, is one of domains or under one, ignoring case and
// the trailing dot.
func under(name string, domains []string) bool {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSuffix(d, "."))
		if d != "" && (name == d || strings.HasSuffix(name, "."+d)) {
			return true
		}
	}
	return false
}

// Handler answers A with 127.0.0.1 and AAAA with ::1 for a name under a domain that domains lists,
// and every other type with an empty NOERROR. It calls domains on every query, so a domain set
// up while serve runs is answered at once. A message with any other name, or one asked while
// domains fails or lists nothing, is REFUSED, so the resolver asks another server.
func Handler(domains func() ([]string, error)) dns.HandlerFunc {
	return func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		if !answers(r, domains) {
			m.Rcode = dns.RcodeRefused
			w.WriteMsg(m)
			return
		}
		m.Authoritative = true
		for _, q := range r.Question {
			hdr := dns.RR_Header{Name: q.Name, Rrtype: q.Qtype, Class: dns.ClassINET, Ttl: TTL}
			switch q.Qtype {
			case dns.TypeA:
				m.Answer = append(m.Answer, &dns.A{Hdr: hdr, A: net.IPv4(127, 0, 0, 1)})
			case dns.TypeAAAA:
				m.Answer = append(m.Answer, &dns.AAAA{Hdr: hdr, AAAA: net.IPv6loopback})
			}
		}
		w.WriteMsg(m)
	}
}

// answers reports whether r has questions and every one is under a domain domains lists.
func answers(r *dns.Msg, domains func() ([]string, error)) bool {
	if len(r.Question) == 0 {
		return false
	}
	ds, err := domains()
	if err != nil {
		return false
	}
	for _, q := range r.Question {
		if !under(q.Name, ds) {
			return false
		}
	}
	return true
}

// Server is Handler listening on one address over UDP and TCP.
type Server struct {
	udp, tcp *dns.Server
	addr     string
}

// Listen binds addr over UDP and TCP and starts answering for domains (Routed, outside tests).
// With port 0, TCP takes the port UDP was given.
func Listen(addr string, domains func() ([]string, error)) (*Server, error) {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("DNS listener: %w", err)
	}
	addr = pc.LocalAddr().String()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("DNS listener: %w", err)
	}
	s := &Server{
		udp:  &dns.Server{PacketConn: pc, Handler: Handler(domains)},
		tcp:  &dns.Server{Listener: ln, Handler: Handler(domains)},
		addr: addr,
	}
	for _, srv := range []*dns.Server{s.udp, s.tcp} {
		started := make(chan struct{})
		srv.NotifyStartedFunc = func() { close(started) }
		go srv.ActivateAndServe()
		<-started
	}
	return s, nil
}

// Addr is the address both listeners are bound to.
func (s *Server) Addr() string { return s.addr }

// Close stops both listeners.
func (s *Server) Close() error {
	return errors.Join(s.udp.Shutdown(), s.tcp.Shutdown())
}
