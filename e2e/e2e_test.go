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
	in, err := startInstance(root, "")
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
