// Package doctor diagnoses why a devingress URL does not load. Every check is
// read-only: it may dial serve and the registered upstreams, but it never
// writes state or config, never loads config into a running serve, and never
// runs an external command.
package doctor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/WonderPanda/devingress/internal/config"
	"github.com/WonderPanda/devingress/internal/probe"
	"github.com/WonderPanda/devingress/internal/resolved"
	"github.com/WonderPanda/devingress/internal/routefile"
	"github.com/WonderPanda/devingress/internal/service"
	"github.com/WonderPanda/devingress/internal/state"
)

// Status is one check's outcome.
type Status string

const (
	OK   Status = "ok"
	Warn Status = "warn"
	Fail Status = "fail"
)

// Check is one line of doctor's output.
type Check struct {
	Check   string `json:"check"`
	Subject string `json:"subject"`
	Status  Status `json:"status"`
	Message string `json:"message"`
	Fix     string `json:"fix"`
}

// Iface is one network interface's name and addresses, enough to decide
// whether a bind address exists on this machine.
type Iface struct {
	Name  string
	Addrs []net.IP
}

// Deps are the dependencies each check reaches outward through, so every
// pass and fail path is unit-testable without a network or root.
type Deps struct {
	Lookup     func(host string) ([]net.IP, error)
	Dial       func(network, address string, timeout time.Duration) (net.Conn, error)
	DialTLS    func(network, address string, conf *tls.Config) (*tls.Conn, error)
	CertPool   func() (*x509.CertPool, error)
	Interfaces func() ([]Iface, error)
	ReadFile   func(name string) ([]byte, error)
	Executable func() (string, error)
	Exchange   func(m *dns.Msg, addr string) (*dns.Msg, error)
	Glob       func(pattern string) ([]string, error)
}

// DefaultDeps reach the real network, filesystem and this machine's
// interfaces and trust store.
func DefaultDeps() Deps {
	return Deps{
		Lookup: func(host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(context.Background(), "ip", host)
		},
		Dial: func(network, address string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout(network, address, timeout)
		},
		DialTLS: func(network, address string, conf *tls.Config) (*tls.Conn, error) {
			return tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, network, address, conf)
		},
		CertPool:   x509.SystemCertPool,
		Interfaces: interfaces,
		ReadFile:   os.ReadFile,
		Executable: func() (string, error) {
			exe, err := os.Executable()
			if err != nil {
				return "", err
			}
			return filepath.EvalSymlinks(exe)
		},
		Exchange: func(m *dns.Msg, addr string) (*dns.Msg, error) {
			r, _, err := (&dns.Client{Timeout: 2 * time.Second}).Exchange(m, addr)
			return r, err
		},
		Glob: filepath.Glob,
	}
}

func interfaces() ([]Iface, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]Iface, 0, len(ifs))
	for _, ifc := range ifs {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		var ips []net.IP
		for _, a := range addrs {
			if ipNet, ok := a.(*net.IPNet); ok {
				ips = append(ips, ipNet.IP)
			}
		}
		out = append(out, Iface{Name: ifc.Name, Addrs: ips})
	}
	return out, nil
}

// Run executes every check and returns them in table order, never nil. A
// config.toml that does not parse stops everything else: nothing downstream
// can be trusted without it.
func Run(deps Deps) []Check {
	checks := []Check{}

	cfg, err := config.Load()
	checks = append(checks, checkConfig(config.ConfigFile(), err))
	if err != nil {
		return checks
	}

	checks = append(checks, checkServe(deps, config.AdminSocket()))
	checks = append(checks, checkPorts(deps, cfg.Bind, cfg.HTTPPort, cfg.HTTPSPort)...)
	checks = append(checks, checkBind(deps, cfg.Bind)...)
	if cfg.DNSListen != "" {
		checks = append(checks, checkDNSListen(deps, cfg.DNSListen))
	}

	rootPath, mkcert, err := cfg.RootCert()
	if err != nil {
		checks = append(checks, Check{Check: "CA", Subject: rootPath, Status: Warn, Message: err.Error(), Fix: "devingress trust"})
	} else {
		checks = append(checks, checkCA(deps, rootPath, mkcert))
	}

	owners, err := state.New(config.StateDir()).List()
	if err != nil {
		checks = append(checks, Check{Check: "owners", Subject: config.StateDir(), Status: Fail, Message: err.Error()})
	} else {
		checks = append(checks, checkOwners(owners)...)
		checks = append(checks, checkHosts(deps, owners, cfg.HTTPSPort, rootPath)...)
		checks = append(checks, checkDNSRoutes(deps, owners)...)
		checks = append(checks, checkUpstreams(deps, owners)...)
	}

	checks = append(checks, checkService(deps)...)
	return checks
}

func checkConfig(path string, err error) Check {
	if err != nil {
		return Check{Check: "config", Subject: path, Status: Fail, Message: "does not parse", Fix: err.Error()}
	}
	return Check{Check: "config", Subject: path, Status: OK, Message: "parses"}
}

func checkServe(deps Deps, socket string) Check {
	c, err := deps.Dial("unix", socket, time.Second)
	if err != nil {
		return Check{Check: "serve", Subject: socket, Status: Fail, Message: "admin socket does not answer",
			Fix: `start it, e.g. "devingress service install", or run "devingress serve" in the foreground`}
	}
	c.Close()
	return Check{Check: "serve", Subject: socket, Status: OK, Message: "admin socket answers"}
}

func checkPorts(deps Deps, bind []string, httpPort, httpsPort int) []Check {
	var out []Check
	for _, addr := range bind {
		for _, p := range []struct {
			name string
			port int
		}{{"http", httpPort}, {"https", httpsPort}} {
			target := net.JoinHostPort(addr, fmt.Sprint(p.port))
			subject := fmt.Sprintf("%s (%s_port)", target, p.name)
			c, err := deps.Dial("tcp", target, 2*time.Second)
			if err != nil {
				out = append(out, Check{Check: "ports", Subject: subject, Status: Fail,
					Message: "no listener accepts TCP on " + target,
					Fix:     "start serve, or free " + target + " if something else is using it"})
				continue
			}
			c.Close()
			out = append(out, Check{Check: "ports", Subject: subject, Status: OK, Message: "accepts TCP"})
		}
	}
	return out
}

func checkBind(deps Deps, bind []string) []Check {
	ifaces, err := deps.Interfaces()
	if err != nil {
		return []Check{{Check: "bind", Subject: "interfaces", Status: Fail, Message: err.Error()}}
	}
	have := map[string]bool{}
	var docker4 string
	for _, ifc := range ifaces {
		for _, ip := range ifc.Addrs {
			have[ip.String()] = true
			if ifc.Name == "docker0" && ip.To4() != nil {
				docker4 = ip.String()
			}
		}
	}
	var out []Check
	for _, b := range bind {
		if have[b] {
			out = append(out, Check{Check: "bind", Subject: b, Status: OK, Message: "bound to an interface on this machine"})
			continue
		}
		out = append(out, Check{Check: "bind", Subject: b, Status: Fail,
			Message: b + " is not an address of any interface on this machine",
			Fix:     fmt.Sprintf("change bind in config.toml: %q is not reachable here", b)})
	}
	if docker4 != "" && !slices.Contains(bind, docker4) {
		out = append(out, Check{Check: "bind", Subject: "docker0", Status: Warn,
			Message: fmt.Sprintf("docker0 (%s) exists but is not in bind", docker4),
			Fix:     fmt.Sprintf("add %q to bind in config.toml so containers reach devingress via host.docker.internal", docker4)})
	}
	return out
}

func checkDNSListen(deps Deps, addr string) Check {
	q := new(dns.Msg).SetQuestion("doctor.devingress.", dns.TypeA)
	r, err := deps.Exchange(q, addr)
	if err != nil {
		return Check{Check: "dns", Subject: addr + " (dns_listen)", Status: Fail,
			Message: "DNS listener does not answer: " + err.Error(),
			Fix:     "start serve, or free " + addr + " if something else is using it"}
	}
	if len(r.Answer) != 1 || r.Answer[0].Header().Rrtype != dns.TypeA || !r.Answer[0].(*dns.A).A.IsLoopback() {
		return Check{Check: "dns", Subject: addr + " (dns_listen)", Status: Fail,
			Message: fmt.Sprintf("answers %v, not 127.0.0.1: something other than devingress listens here", r.Answer),
			Fix:     "free " + addr + ", or set dns_listen in config.toml to a free address"}
	}
	return Check{Check: "dns", Subject: addr + " (dns_listen)", Status: OK, Message: "DNS listener answers 127.0.0.1"}
}

func checkCA(deps Deps, rootPath string, mkcert bool) Check {
	fix := "devingress trust"
	if mkcert {
		fix = "mkcert -install"
	}
	data, err := deps.ReadFile(rootPath)
	if err != nil {
		return Check{Check: "CA", Subject: rootPath, Status: Warn, Message: "root certificate does not exist yet", Fix: fix}
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return Check{Check: "CA", Subject: rootPath, Status: Warn, Message: "root certificate is not valid PEM", Fix: fix}
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return Check{Check: "CA", Subject: rootPath, Status: Warn, Message: err.Error(), Fix: fix}
	}
	pool, err := deps.CertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return Check{Check: "CA", Subject: rootPath, Status: Warn, Message: "root certificate is not trusted system-wide", Fix: fix}
	}
	return Check{Check: "CA", Subject: rootPath, Status: OK, Message: "root certificate is trusted system-wide"}
}

func checkOwners(owners []routefile.Owner) []Check {
	var out []Check
	for _, o := range owners {
		if _, err := os.Stat(o.Dir); err != nil {
			out = append(out, Check{Check: "owners", Subject: o.Dir, Status: Warn,
				Message: "owner directory is gone", Fix: "pruned on the next up or down"})
			continue
		}
		out = append(out, Check{Check: "owners", Subject: o.Dir, Status: OK, Message: "owner directory exists"})
	}
	return out
}

func checkHosts(deps Deps, owners []routefile.Owner, httpsPort int, rootPath string) []Check {
	rootPEM, rootErr := deps.ReadFile(rootPath)
	var rootPool *x509.CertPool
	if rootErr == nil {
		rootPool = x509.NewCertPool()
		if !rootPool.AppendCertsFromPEM(rootPEM) {
			rootErr = fmt.Errorf("%s: not a valid certificate", rootPath)
		}
	}

	seen := map[string]bool{}
	var out []Check
	for _, o := range owners {
		if _, err := os.Stat(o.Dir); err != nil {
			continue // covered by the owners check
		}
		for _, s := range o.Sites {
			if seen[s.Host] {
				continue
			}
			seen[s.Host] = true
			out = append(out, checkHostDNS(deps, s.Host))
			out = append(out, checkHostTLS(deps, s.Host, httpsPort, rootPool, rootErr))
		}
	}
	return out
}

func checkHostDNS(deps Deps, host string) Check {
	fix := dnsFix(host)
	ips, err := deps.Lookup(host)
	if err != nil {
		return Check{Check: "host", Subject: host, Status: Fail, Message: "does not resolve: " + err.Error(), Fix: fix}
	}
	if !slices.ContainsFunc(ips, net.IP.IsLoopback) {
		return Check{Check: "host", Subject: host, Status: Fail,
			Message: fmt.Sprintf("resolves to %v, not loopback", ips), Fix: fix}
	}
	return Check{Check: "host", Subject: host, Status: OK, Message: "resolves to loopback"}
}

// dnsFix suggests routing host's parent domain to devingress, the likeliest domain it was
// registered under.
func dnsFix(host string) string {
	domain := host
	if _, parent, ok := strings.Cut(host, "."); ok {
		domain = parent
	}
	return fmt.Sprintf("devingress dns setup %s, or point DNS at 127.0.0.1 (dnsmasq, /etc/hosts, *.localhost)", domain)
}

// checkDNSRoutes warns about each live host that neither a devingress drop-in routes to the DNS
// listener nor resolves to loopback some other way.
func checkDNSRoutes(deps Deps, owners []routefile.Owner) []Check {
	routed := map[string]string{} // domain -> the devingress drop-in routing it
	paths, _ := deps.Glob(filepath.Join(resolved.Dir, resolved.DropInName("*")))
	for _, p := range paths {
		data, err := deps.ReadFile(p)
		if err != nil {
			continue
		}
		domain := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), "devingress-"), ".conf")
		if resolved.RoutesDomain(string(data), domain) {
			routed[domain] = p
		}
	}

	seen := map[string]bool{}
	var out []Check
	for _, o := range owners {
		if _, err := os.Stat(o.Dir); err != nil {
			continue // covered by the owners check
		}
		for _, s := range o.Sites {
			if seen[s.Host] {
				continue
			}
			seen[s.Host] = true
			out = append(out, checkDNSRoute(deps, s.Host, routed))
		}
	}
	return out
}

func checkDNSRoute(deps Deps, host string, routed map[string]string) Check {
	for d := host; d != ""; {
		if p, ok := routed[d]; ok {
			return Check{Check: "dns", Subject: host, Status: OK, Message: "routed to the DNS listener by " + p}
		}
		_, d, _ = strings.Cut(d, ".")
	}
	if ips, err := deps.Lookup(host); err == nil && slices.ContainsFunc(ips, net.IP.IsLoopback) {
		return Check{Check: "dns", Subject: host, Status: OK, Message: "no devingress drop-in, but resolves to loopback"}
	}
	return Check{Check: "dns", Subject: host, Status: Warn,
		Message: "no devingress drop-in in " + resolved.Dir + " routes it, and it does not resolve to loopback",
		Fix:     dnsFix(host)}
}

// checkUpstreams dials every live owner's upstreams in parallel and warns about each one nothing
// listens on: the usual reason a registered URL 502s.
func checkUpstreams(deps Deps, owners []routefile.Owner) []Check {
	var live []routefile.Owner
	var upstreams []string
	for _, o := range owners {
		if _, err := os.Stat(o.Dir); err != nil {
			continue // covered by the owners check
		}
		live = append(live, o)
		for _, s := range o.Sites {
			upstreams = append(upstreams, s.Upstream)
		}
		for _, p := range o.Paths {
			upstreams = append(upstreams, p.Upstream)
		}
	}
	results := probe.All(deps.Dial, upstreams)

	var out []Check
	add := func(o routefile.Owner, upstream, route string) {
		subject := fmt.Sprintf("%s (%s)", upstream, route)
		switch results[upstream] {
		case probe.Up:
			out = append(out, Check{Check: "upstream", Subject: subject, Status: OK, Message: "accepts TCP"})
		case probe.Down:
			out = append(out, Check{Check: "upstream", Subject: subject, Status: Warn,
				Message: "nothing listening", Fix: "start the dev server for " + o.Dir})
		}
	}
	for _, o := range live {
		for _, s := range o.Sites {
			add(o, s.Upstream, s.Host)
		}
		for _, p := range o.Paths {
			add(o, p.Upstream, p.Prefix)
		}
	}
	return out
}

func checkHostTLS(deps Deps, host string, httpsPort int, rootPool *x509.CertPool, rootErr error) Check {
	const fix = "devingress trust"
	if rootErr != nil {
		return Check{Check: "host", Subject: host, Status: Fail, Message: "cannot verify: " + rootErr.Error(), Fix: fix}
	}
	addr := net.JoinHostPort("127.0.0.1", fmt.Sprint(httpsPort))
	conn, err := deps.DialTLS("tcp", addr, &tls.Config{ServerName: host, RootCAs: rootPool})
	if err != nil {
		return Check{Check: "host", Subject: host, Status: Fail,
			Message: fmt.Sprintf("TLS handshake to %s failed: %v", addr, err), Fix: fix}
	}
	conn.Close()
	return Check{Check: "host", Subject: host, Status: OK, Message: "serves a certificate that verifies"}
}

func checkService(deps Deps) []Check {
	data, err := deps.ReadFile(service.UnitPath)
	if err != nil {
		return nil // not installed: nothing to check
	}
	execStart := unitExecStart(string(data))
	exe, exeErr := deps.Executable()
	want := exe + " serve"
	switch {
	case execStart == "":
		return []Check{{Check: "service", Subject: service.UnitPath, Status: Warn,
			Message: "no ExecStart line found", Fix: `rerun "devingress service install"`}}
	case exeErr != nil:
		return []Check{{Check: "service", Subject: service.UnitPath, Status: Warn,
			Message: exeErr.Error(), Fix: `rerun "devingress service install"`}}
	case execStart != want:
		return []Check{{Check: "service", Subject: service.UnitPath, Status: Warn,
			Message: fmt.Sprintf("ExecStart is %q, not the running binary (%q)", execStart, want),
			Fix:     `rerun "devingress service install"`}}
	}
	return []Check{{Check: "service", Subject: service.UnitPath, Status: OK, Message: "ExecStart matches the running binary"}}
}

func unitExecStart(unit string) string {
	for line := range strings.SplitSeq(unit, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "ExecStart="); ok {
			return v
		}
	}
	return ""
}
