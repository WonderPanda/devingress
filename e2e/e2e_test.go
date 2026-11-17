//go:build e2e

package e2e

import (
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

var (
	webHost = "web-e2e-branch." + domain
	h1Host  = "h1-e2e-branch." + domain
)

func webRoute() string {
	return fmt.Sprintf("domain = %q\n\n[[route]]\nhost = \"web-${slug}\"\nto = %q\n", domain, upstreamPort())
}

func TestServes(t *testing.T) {
	srv.waitServes(t, pool(t, filepath.Join(caDir, "rootCA.pem")), webHost)
}

func TestCertificate(t *testing.T) {
	roots := pool(t, filepath.Join(caDir, "rootCA.pem"))
	ca := readCert(t, filepath.Join(caDir, "rootCA.pem"))
	leaves := map[string]*x509.Certificate{}
	for _, host := range []string{webHost, h1Host} {
		srv.waitServes(t, roots, host)
		cs, err := srv.handshake(roots, host)
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		leaf := cs.PeerCertificates[0]
		if len(cs.VerifiedChains) == 0 {
			t.Fatalf("%s: no chain to the test CA", host)
		}
		if got := cs.VerifiedChains[0][len(cs.VerifiedChains[0])-1]; !got.Equal(ca) {
			t.Fatalf("%s: chain ends at %s, not the test CA", host, got.Subject)
		}
		if !slices.Equal(leaf.DNSNames, []string{host}) || len(leaf.IPAddresses)+len(leaf.URIs)+len(leaf.EmailAddresses) > 0 {
			t.Fatalf("%s: SAN is DNS %v IP %v URI %v email %v", host, leaf.DNSNames, leaf.IPAddresses, leaf.URIs, leaf.EmailAddresses)
		}
		inter := x509.NewCertPool()
		for _, c := range cs.PeerCertificates[1:] {
			inter.AddCert(c)
		}
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: host, Intermediates: inter}); err == nil {
			t.Fatalf("%s: leaf verified against the system roots", host)
		}
		leaves[host] = leaf
	}
	if leaves[webHost].Equal(leaves[h1Host]) {
		t.Fatal("both hosts got the same leaf")
	}
}

func TestALPN(t *testing.T) {
	roots := pool(t, filepath.Join(caDir, "rootCA.pem"))
	for host, want := range map[string]string{webHost: "h2", h1Host: "http/1.1"} {
		srv.waitServes(t, roots, host)
		cs, err := srv.handshake(roots, host)
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		if cs.NegotiatedProtocol != want {
			t.Errorf("%s: negotiated %q, want %q", host, cs.NegotiatedProtocol, want)
		}
	}
}

// TestDoctor checks `devingress doctor --json` against the harness's live instance: serve, ports
// and the TLS half of the per-host check should all pass, while the DNS half fails because the
// harness's domain never resolves anywhere, which must also make the exit code 1.
func TestDoctor(t *testing.T) {
	roots := pool(t, filepath.Join(caDir, "rootCA.pem"))
	srv.waitServes(t, roots, webHost)
	srv.waitServes(t, roots, h1Host)

	out, err := srv.run(mainDir, "doctor", "--json")
	if err == nil {
		t.Fatalf("doctor --json: exit 0, want 1 (the harness's domain never resolves)\n%s", out)
	}
	type check struct {
		Check   string `json:"check"`
		Subject string `json:"subject"`
		Status  string `json:"status"`
	}
	var checks []check
	if err := json.Unmarshal([]byte(out), &checks); err != nil {
		t.Fatalf("doctor --json: %v\n%s", err, out)
	}

	byCheck := map[string][]string{}
	for _, c := range checks {
		byCheck[c.Check] = append(byCheck[c.Check], c.Status)
	}
	if !slices.ContainsFunc(checks, func(c check) bool {
		return c.Check == "dns" && c.Subject == srv.dnsAddr+" (dns_listen)" && c.Status == "ok"
	}) {
		t.Errorf("no passing DNS listener check for %s in:\n%s", srv.dnsAddr, out)
	}
	for _, name := range []string{"serve", "ports"} {
		if len(byCheck[name]) == 0 {
			t.Errorf("no %s checks in:\n%s", name, out)
		}
		for _, status := range byCheck[name] {
			if status != "ok" {
				t.Errorf("%s: want every status ok, got %v", name, byCheck[name])
			}
		}
	}

	hosts := map[string][]string{}
	for _, c := range checks {
		if c.Check == "host" {
			hosts[c.Subject] = append(hosts[c.Subject], c.Status)
		}
	}
	for _, host := range []string{webHost, h1Host} {
		got := hosts[host]
		if len(got) != 2 {
			t.Fatalf("host %s: %d entries, want 2 (DNS and TLS):\n%s", host, len(got), out)
		}
		oks, fails := 0, 0
		for _, status := range got {
			switch status {
			case "ok":
				oks++
			case "fail":
				fails++
			}
		}
		if oks != 1 || fails != 1 {
			t.Errorf("host %s: statuses %v, want exactly one ok (TLS) and one fail (DNS)", host, got)
		}
	}

	// The harness's hello-world upstream is listening, so both routes to it probe ok.
	for _, host := range []string{webHost, h1Host} {
		subject := fmt.Sprintf("127.0.0.1:%s (%s)", upstreamPort(), host)
		if !slices.ContainsFunc(checks, func(c check) bool {
			return c.Check == "upstream" && c.Subject == subject && c.Status == "ok"
		}) {
			t.Errorf("no passing upstream check %q in:\n%s", subject, out)
		}
	}
}

// TestRoutesURLsConnect checks that `routes --json` prints URLs that are
// ready to use as-is, on an instance whose https_port is not 443 (the
// harness always picks a free port, so this covers that bug by construction).
func TestRoutesURLsConnect(t *testing.T) {
	roots := pool(t, filepath.Join(caDir, "rootCA.pem"))
	srv.waitServes(t, roots, webHost)

	out, err := srv.run(mainDir, "routes", "--json")
	if err != nil {
		t.Fatalf("routes --json: %v\n%s", err, out)
	}
	var routes []struct {
		Kind string `json:"kind"`
		URL  string `json:"url"`
		Host string `json:"host"`
	}
	if err := json.Unmarshal([]byte(out), &routes); err != nil {
		t.Fatalf("routes --json: %v\n%s", err, out)
	}
	port := fmt.Sprintf(":%d", srv.httpsPort)
	found := false
	for _, r := range routes {
		if r.Kind != "site" || r.Host != webHost {
			continue
		}
		found = true
		if !strings.HasSuffix(r.URL, port) {
			t.Fatalf("url %q does not carry the instance's non-default https_port %s", r.URL, port)
		}
		resp, err := srv.client(roots).Get(r.URL)
		if err != nil {
			t.Fatalf("GET %s: %v", r.URL, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("GET %s: %v", r.URL, err)
		}
		if resp.StatusCode != http.StatusOK || string(body) != hello {
			t.Fatalf("GET %s: got %s %q", r.URL, resp.Status, body)
		}
	}
	if !found {
		t.Fatalf("no site route for %s in:\n%s", webHost, out)
	}
}

func TestDown(t *testing.T) {
	roots := pool(t, filepath.Join(caDir, "rootCA.pem"))
	dir := checkout(t, srv, "down-branch", webRoute())
	host := "web-down-branch." + domain
	up(t, srv, dir)
	srv.waitServes(t, roots, host)

	if out, err := srv.run(dir, "down"); err != nil {
		t.Fatalf("down: %v\n%s", err, out)
	}
	srv.waitGone(t, roots, host)
	out, err := srv.run(dir, "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if strings.Contains(out, dir) || strings.Contains(out, host) {
		t.Fatalf("status still lists %s:\n%s", dir, out)
	}
	srv.waitServes(t, roots, webHost)
}

// TestNamedOwner: one checkout registers a second route set with --name; removing it leaves the
// checkout's unnamed set serving.
func TestNamedOwner(t *testing.T) {
	roots := pool(t, filepath.Join(caDir, "rootCA.pem"))
	dir := checkout(t, srv, "named-branch", webRoute())
	coding := fmt.Sprintf("domain = %q\n\n[[route]]\nhost = \"coding-${slug}\"\nto = %q\n", domain, upstreamPort())
	if err := os.WriteFile(filepath.Join(dir, "devingress.coding.toml"), []byte(coding), 0o644); err != nil {
		t.Fatal(err)
	}
	mainHost, codingHost := "web-named-branch."+domain, "coding-named-branch."+domain
	up(t, srv, dir)
	if out, err := srv.run(dir, "up", "--name", "coding", "--file", "devingress.coding.toml"); err != nil {
		t.Fatalf("up --name coding: %v\n%s", err, out)
	}
	srv.waitServes(t, roots, mainHost)
	srv.waitServes(t, roots, codingHost)

	if out, err := srv.run(dir, "down", "--name", "coding"); err != nil || !strings.Contains(out, "removed "+dir+" [coding]") {
		t.Fatalf("down --name coding: %v\n%s", err, out)
	}
	srv.waitGone(t, roots, codingHost)
	if err := srv.serves(roots, mainHost); err != nil {
		t.Fatalf("%s stopped serving after down --name coding: %v", mainHost, err)
	}

	if out, err := srv.run(dir, "down", "--all"); err != nil {
		t.Fatalf("down --all: %v\n%s", err, out)
	}
	srv.waitGone(t, roots, mainHost)
}

func TestPrune(t *testing.T) {
	roots := pool(t, filepath.Join(caDir, "rootCA.pem"))
	dir := checkout(t, srv, "prune-branch", webRoute())
	host := "web-prune-branch." + domain
	up(t, srv, dir)
	srv.waitServes(t, roots, host)

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if out := up(t, srv, mainDir); !strings.Contains(out, "pruned "+dir) {
		t.Fatalf("up did not report pruning %s:\n%s", dir, out)
	}
	srv.waitGone(t, roots, host)
	srv.waitServes(t, roots, webHost)
}

func TestConflict(t *testing.T) {
	roots := pool(t, filepath.Join(caDir, "rootCA.pem"))
	// Literal host, so it claims the main checkout's web host.
	dir := checkout(t, srv, "conflict-branch",
		fmt.Sprintf("domain = %q\n\n[[route]]\nhost = \"web-e2e-branch\"\nto = %q\n", domain, upstreamPort()))
	out, err := srv.run(dir, "up")
	if err == nil {
		t.Fatalf("up claiming %s succeeded:\n%s", webHost, out)
	}
	if !strings.Contains(out, "claimed by both") {
		t.Fatalf("up failed for another reason: %v\n%s", err, out)
	}
	if err := srv.serves(roots, webHost); err != nil {
		t.Fatalf("%s stopped serving after the rejected up: %v", webHost, err)
	}
	out, err = srv.run(dir, "status")
	if err != nil || strings.Contains(out, dir) {
		t.Fatalf("status lists the rejected checkout (err %v):\n%s", err, out)
	}
}

// TestCaddyOwnRoot runs its own serve with no ca_root (and no CAROOT, with
// HOME in the temp root so `mkcert -CAROOT` finds nothing): Caddy mints a
// root under $XDG_DATA_HOME and signs with it.
func TestCaddyOwnRoot(t *testing.T) {
	root, err := os.MkdirTemp("", "di")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	in, err := startInstance(root, "", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(in.stop)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("serve log:\n%s", in.log.String())
		}
	})

	dir := checkout(t, in, "own-root", webRoute())
	host := "web-own-root." + domain
	up(t, in, dir)

	caddyRoot := filepath.Join(root, "data", "devingress", "caddy", "pki", "authorities", "local", "root.crt")
	if err := poll(30*time.Second, func() error { _, err := os.Stat(caddyRoot); return err }); err != nil {
		t.Fatal(err)
	}
	roots := pool(t, caddyRoot)
	in.waitServes(t, roots, host)
	if err := in.serves(pool(t, filepath.Join(caDir, "rootCA.pem")), host); err == nil {
		t.Fatal("served a leaf the other instance's test CA verifies")
	}
}

// TestDNS queries the instance's DNS listener the way systemd-resolved would when its global
// scope is the default route (#38): a registered host under a domain "devingress dns setup" never
// routed, and a public name, both get REFUSED with no records over UDP and TCP, so the resolver
// moves on instead of caching loopback. The harness cannot write the system's drop-in or resolver
// directory, so the in-domain answers are covered by internal/localdns's tests.
func TestDNS(t *testing.T) {
	for _, network := range []string{"udp", "tcp"} {
		for _, name := range []string{webHost, "api.github.com"} {
			for _, qtype := range []uint16{dns.TypeA, dns.TypeAAAA} {
				q := new(dns.Msg).SetQuestion(dns.Fqdn(name), qtype)
				r, _, err := (&dns.Client{Net: network}).Exchange(q, srv.dnsAddr)
				if err != nil {
					t.Fatalf("%s %s %s: %v", network, name, dns.TypeToString[qtype], err)
				}
				if r.Rcode != dns.RcodeRefused || len(r.Answer) != 0 {
					t.Errorf("%s %s %s: %s %v, want REFUSED and no answer", network, name, dns.TypeToString[qtype],
						dns.RcodeToString[r.Rcode], r.Answer)
				}
			}
		}
	}
}

// TestDNSListenInUse starts a second serve whose dns_listen is the harness instance's: it must
// exit instead of serving, naming the address.
func TestDNSListenInUse(t *testing.T) {
	root, err := os.MkdirTemp("", "di")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	in, err := startInstance(root, caDir, srv.dnsAddr)
	if err == nil {
		in.stop()
		t.Fatalf("serve started with dns_listen %s already in use", srv.dnsAddr)
	}
	if !strings.Contains(err.Error(), "serve exited") || !strings.Contains(err.Error(), srv.dnsAddr) {
		t.Errorf("error does not name %s:\n%v", srv.dnsAddr, err)
	}
	t.Log(err)
}

// TestUnreachableUpstream registers a site and two path routes (strip and not) whose upstream port
// is closed: each answers 502 with a plain-text body naming the upstream, what it serves and the
// checkout that owns it.
func TestUnreachableUpstream(t *testing.T) {
	roots := pool(t, filepath.Join(caDir, "rootCA.pem"))
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	dir := checkout(t, srv, "dead-branch", fmt.Sprintf(
		"domain = %q\n\n[[route]]\nhost = \"web-${slug}\"\nto = \"%d\"\n\n"+
			"[[path]]\nprefix = \"/dead-strip\"\nto = \"%d\"\nstrip = true\n\n"+
			"[[path]]\nprefix = \"/dead-keep\"\nto = \"%d\"\n",
		domain, port, port, port))
	up(t, srv, dir)
	host := "web-dead-branch." + domain
	upstream := fmt.Sprintf("127.0.0.1:%d", port)

	for _, c := range []struct {
		client *http.Client
		url    string
		names  string
	}{
		{srv.client(roots), "https://" + host + "/", host},
		{http.DefaultClient, fmt.Sprintf("http://127.0.0.1:%d/dead-strip/a", srv.httpPort), "/dead-strip/*"},
		{http.DefaultClient, fmt.Sprintf("http://127.0.0.1:%d/dead-keep/a", srv.httpPort), "/dead-keep/*"},
	} {
		want := fmt.Sprintf("nothing answered at %s for %s (owner %s)", upstream, c.names, dir)
		err := poll(10*time.Second, func() error {
			resp, err := c.client.Get(c.url)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return err
			}
			ct := resp.Header.Get("Content-Type")
			if resp.StatusCode != http.StatusBadGateway || ct != "text/plain; charset=utf-8" || !strings.Contains(string(body), want) {
				return fmt.Errorf("got %s, Content-Type %q, body %q; want 502, text/plain, a body containing %q",
					resp.Status, ct, body, want)
			}
			return nil
		})
		if err != nil {
			t.Errorf("GET %s: %v", c.url, err)
		}
	}
}
