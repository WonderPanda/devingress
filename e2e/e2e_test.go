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

// TestDNS queries the instance's DNS listener the way systemd-resolved would, for a registered
// host: A is 127.0.0.1 and AAAA ::1, over UDP and TCP.
func TestDNS(t *testing.T) {
	for _, network := range []string{"udp", "tcp"} {
		for qtype, want := range map[uint16]string{dns.TypeA: "127.0.0.1", dns.TypeAAAA: "::1"} {
			q := new(dns.Msg).SetQuestion(dns.Fqdn(webHost), qtype)
			r, _, err := (&dns.Client{Net: network}).Exchange(q, srv.dnsAddr)
			if err != nil {
				t.Fatalf("%s %s: %v", network, dns.TypeToString[qtype], err)
			}
			var got []string
			for _, rr := range r.Answer {
				switch rr := rr.(type) {
				case *dns.A:
					got = append(got, rr.A.String())
				case *dns.AAAA:
					got = append(got, rr.AAAA.String())
				}
			}
			if r.Rcode != dns.RcodeSuccess || !slices.Equal(got, []string{want}) {
				t.Errorf("%s %s %s: %s %v, want %s", network, webHost, dns.TypeToString[qtype],
					dns.RcodeToString[r.Rcode], got, want)
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
