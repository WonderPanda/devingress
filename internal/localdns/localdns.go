// Package localdns is serve's wildcard DNS listener: every name is loopback. It only receives
// queries for the domains "devingress dns setup" routes to it, so it needs no domain list.
package localdns

import (
	"errors"
	"fmt"
	"net"

	"github.com/miekg/dns"
)

// TTL is the lifetime of every answer, in seconds.
const TTL = 5

// Handle answers A with 127.0.0.1 and AAAA with ::1 for any name, and every other type with an
// empty NOERROR.
func Handle(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(r)
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

// Server is Handle listening on one address over UDP and TCP.
type Server struct {
	udp, tcp *dns.Server
	addr     string
}

// Listen binds addr over UDP and TCP and starts answering. With port 0, TCP takes the port UDP
// was given.
func Listen(addr string) (*Server, error) {
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
		udp:  &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(Handle)},
		tcp:  &dns.Server{Listener: ln, Handler: dns.HandlerFunc(Handle)},
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
