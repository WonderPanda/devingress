package doctor

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/WonderPanda/devingress/internal/config"
	"github.com/WonderPanda/devingress/internal/resolved"
	"github.com/WonderPanda/devingress/internal/routefile"
)

// fakeToken matches caddy-dns/cloudflare's format check; it is no real token.
const fakeToken = "cfut_" + "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN"

const suffix = "machine.example.dev"

var tailnetIP = netip.MustParseAddr("100.64.0.10")

func fixture(t *testing.T, name string) func() ([]byte, error) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return func() ([]byte, error) { return data, nil }
}

func TestCheckTailscaleForward(t *testing.T) {
	raw := func(s string) func() ([]byte, error) { return func() ([]byte, error) { return []byte(s), nil } }
	for _, c := range []struct {
		name   string
		status func() ([]byte, error)
		want   Status
		msg    string
	}{
		{"forward", fixture(t, "serve-status-forward.json"), OK, "forwards raw TCP to 127.0.0.1:18443"},
		{"no forward", fixture(t, "serve-status-no-forward.json"), Fail, "nothing forwards the tailnet's port 443"},
		{"nothing served", raw("{}"), Fail, "nothing forwards"},
		{"wrong port", raw(`{"TCP":{"443":{"TCPForward":"127.0.0.1:443"}}}`), Fail, `forwards to "127.0.0.1:443", not 127.0.0.1:18443`},
		{"tailscale https", raw(`{"TCP":{"443":{"HTTPS":true}}}`), Fail, "Tailscale's own HTTP(S) serve"},
		{"terminates tls", raw(`{"TCP":{"443":{"TCPForward":"127.0.0.1:18443","TerminateTLS":"machine.tail0000.ts.net"}}}`), Fail, "terminates TLS"},
		{"proxy protocol", raw(`{"TCP":{"443":{"TCPForward":"127.0.0.1:18443","ProxyProtocol":2}}}`), Fail, "PROXY protocol"},
		{"not json", raw("Tailscale is stopped."), Fail, "not its JSON"},
		{"no cli", func() ([]byte, error) { return nil, ErrNoTailscale }, Warn, "no tailscale CLI"},
		{"cli error", func() ([]byte, error) { return nil, errors.New("failed to connect to local tailscaled") }, Fail, "local tailscaled"},
	} {
		got := checkTailscaleForward(Deps{TailscaleServeStatus: c.status}, 18443)
		if got.Status != c.want || !strings.Contains(got.Message, c.msg) {
			t.Errorf("%s: %+v, want %s with %q", c.name, got, c.want, c.msg)
		}
		if got.Status != OK && !strings.Contains(got.Fix, "tailscale serve --bg --tcp 443 tcp://127.0.0.1:18443") && c.name != "cli error" {
			t.Errorf("%s: fix %q does not give the command", c.name, got.Fix)
		}
	}
}

func TestCheckTailnetAddress(t *testing.T) {
	ifaces := func(ips ...string) Deps {
		var addrs []net.IP
		for _, ip := range ips {
			addrs = append(addrs, net.ParseIP(ip))
		}
		return Deps{GOOS: "linux", Interfaces: func() ([]Iface, error) { return []Iface{{Name: "tailscale0", Addrs: addrs}}, nil }}
	}
	if ip, c := checkTailnetAddress(ifaces("100.64.0.10")); c.Status != OK || ip != tailnetIP {
		t.Errorf("one: %v %+v", ip, c)
	}
	if ip, c := checkTailnetAddress(ifaces()); c.Status != Fail || ip.IsValid() || !strings.Contains(c.Message, "no tailnet address") {
		t.Errorf("none: %v %+v", ip, c)
	}
	if ip, c := checkTailnetAddress(ifaces("100.64.0.10", "100.64.0.11")); c.Status != Fail || ip.IsValid() || !strings.Contains(c.Message, "several") {
		t.Errorf("several: %v %+v", ip, c)
	}
}

func TestCheckWildcardDNS(t *testing.T) {
	lookup := func(ips ...string) Deps {
		return Deps{Label: func() string { return "devingress-doctor-1" }, GOOS: "linux",
			Glob: func(string) ([]string, error) { return nil, nil },
			Lookup: func(host string) ([]net.IP, error) {
				if host != "devingress-doctor-1."+suffix {
					t.Errorf("looked up %s", host)
				}
				if len(ips) == 0 {
					return nil, errors.New("no such host")
				}
				var out []net.IP
				for _, ip := range ips {
					out = append(out, net.ParseIP(ip))
				}
				return out, nil
			}}
	}
	if c := checkWildcardDNS(lookup("100.64.0.10"), suffix, tailnetIP); c.Status != OK {
		t.Errorf("match: %+v", c)
	}
	if c := checkWildcardDNS(lookup("100.64.0.99"), suffix, tailnetIP); c.Status != Fail || !strings.Contains(c.Message, "not 100.64.0.10") {
		t.Errorf("elsewhere: %+v", c)
	}
	if c := checkWildcardDNS(lookup(), suffix, tailnetIP); c.Status != Fail || !strings.Contains(c.Message, "does not resolve") {
		t.Errorf("nowhere: %+v", c)
	}
	if c := checkWildcardDNS(lookup("100.64.0.10"), suffix, netip.Addr{}); c.Status != Warn {
		t.Errorf("no tailnet address: %+v", c)
	}
	if c := checkWildcardDNS(lookup("127.0.0.1"), suffix, tailnetIP); c.Status != Fail {
		t.Errorf("loopback, not routed: %+v", c)
	}

	// devingress dns setup example.dev makes serve's DNS listener answer for the suffix too.
	routed := lookup("127.0.0.1")
	dropIn := filepath.Join(resolved.Dir, resolved.DropInName("example.dev"))
	routed.Glob = func(string) ([]string, error) { return []string{dropIn}, nil }
	routed.ReadFile = func(string) ([]byte, error) {
		return []byte(resolved.DropIn(config.DefaultDNSListen, "example.dev", false)), nil
	}
	if c := checkWildcardDNS(routed, suffix, tailnetIP); c.Status != Warn || !strings.Contains(c.Message, "routes example.dev to serve's DNS listener") {
		t.Errorf("loopback, routed: %+v", c)
	}
}

// wildcardServer serves a *.<suffix> certificate signed by a throwaway root, expiring at
// notAfter, and returns its address and a pool trusting the root (standing in for the system
// roots).
func wildcardServer(t *testing.T, notAfter time.Time) (string, *x509.CertPool) {
	t.Helper()
	rootPEM, rootCert, rootKey := genCA(t, "doctor remote test root")
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(rootPEM)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "*." + suffix},
		DNSNames:     []string{"*." + suffix},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, rootCert, &key.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der, rootCert.Raw}, PrivateKey: key}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()
	return ln.Addr().String(), pool
}

// certDeps dials every TLS address at real instead, recording what was asked.
func certDeps(real string, pool *x509.CertPool, now time.Time, asked *[]string) Deps {
	return Deps{
		Label:    func() string { return "devingress-doctor-1" },
		Now:      func() time.Time { return now },
		CertPool: func() (*x509.CertPool, error) { return pool, nil },
		DialTLS: func(network, address string, conf *tls.Config) (*tls.Conn, error) {
			*asked = append(*asked, address+" "+conf.ServerName)
			return tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, network, real, conf)
		},
	}
}

func TestCheckRemoteCert(t *testing.T) {
	notAfter := time.Now().Add(60 * 24 * time.Hour).Truncate(time.Second) // x509 keeps whole seconds
	addr, pool := wildcardServer(t, notAfter)
	var asked []string

	c := checkRemoteCert(certDeps(addr, pool, time.Now(), &asked), suffix, "127.0.0.1:18443", "certificate", "fix")
	if c.Status != OK || !strings.Contains(c.Message, "publicly trusted") {
		t.Errorf("valid: %+v", c)
	}
	if asked[0] != "127.0.0.1:18443 devingress-doctor-1."+suffix {
		t.Errorf("dialed %q", asked)
	}

	soon := notAfter.Add(-10 * 24 * time.Hour)
	if c := checkRemoteCert(certDeps(addr, pool, soon, &asked), suffix, "127.0.0.1:18443", "certificate", "fix"); c.Status != Warn || !strings.Contains(c.Message, "in 10 days") {
		t.Errorf("expiring: %+v", c)
	}

	// The local CA's certificate, or any other root's, does not verify against the system roots.
	_, otherPool := wildcardServer(t, notAfter)
	if c := checkRemoteCert(certDeps(addr, otherPool, time.Now(), &asked), suffix, "127.0.0.1:18443", "certificate", "fix"); c.Status != Fail || c.Fix != "fix" {
		t.Errorf("untrusted: %+v", c)
	}

	if c := checkRemoteCert(certDeps("127.0.0.1:1", pool, time.Now(), &asked), suffix, "100.64.0.10:443", "end to end", "fix"); c.Status != Fail {
		t.Errorf("nothing listening: %+v", c)
	}
}

// verifyAPI answers the token verify request with status and body, checking it was asked for
// the token in the file.
func verifyAPI(t *testing.T, status int, body string) func(*http.Request) (*http.Response, error) {
	return func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != tokenVerifyURL || req.Header.Get("Authorization") != "Bearer "+fakeToken {
			t.Errorf("request %s with Authorization %q", req.URL, req.Header.Get("Authorization"))
		}
		return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: io.NopCloser(strings.NewReader(body))}, nil
	}
}

func writeTokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cloudflare-token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckToken(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	path := writeTokenFile(t, fakeToken)
	for _, c := range []struct {
		name string
		do   func(*http.Request) (*http.Response, error)
		want Status
		msg  string
	}{
		{"active", verifyAPI(t, 200, `{"success":true,"result":{"id":"x","status":"active"}}`), OK, "active, no expiry"},
		{"expires later", verifyAPI(t, 200, `{"success":true,"result":{"status":"active","expires_on":"2027-06-01T00:00:00Z"}}`), OK, "expires 2027-06-01"},
		{"expires soon", verifyAPI(t, 200, `{"success":true,"result":{"status":"active","expires_on":"2026-12-07T12:00:00Z"}}`), Warn, "in 10 days"},
		{"disabled", verifyAPI(t, 200, `{"success":true,"result":{"status":"disabled"}}`), Fail, `status "disabled"`},
		{"invalid", verifyAPI(t, 401, `{"success":false,"errors":[{"code":1000,"message":"Invalid API Token"}]}`), Fail, "Invalid API Token"},
		{"echoes the token", verifyAPI(t, 400, `{"success":false,"errors":[{"message":"bad token `+fakeToken+`"}]}`), Fail, "bad token [redacted token]"},
		{"unreachable", func(*http.Request) (*http.Response, error) { return nil, errors.New("dial tcp: no route to host") }, Fail, "cannot reach Cloudflare"},
	} {
		got := checkToken(Deps{HTTPDo: c.do, Now: func() time.Time { return now }}, path)
		if got.Status != c.want || !strings.Contains(got.Message, c.msg) {
			t.Errorf("%s: %+v, want %s with %q", c.name, got, c.want, c.msg)
		}
		if strings.Contains(got.Message+got.Fix+got.Subject, fakeToken) {
			t.Errorf("%s: output quotes the token: %+v", c.name, got)
		}
	}

	account := writeTokenFile(t, "cfat_"+strings.Repeat("a", 40))
	if got := checkToken(Deps{HTTPDo: func(*http.Request) (*http.Response, error) {
		t.Error("an account token was sent to the user verify endpoint")
		return nil, errors.New("no")
	}}, account); got.Status != Warn {
		t.Errorf("account token: %+v", got)
	}
}

func TestCheckTwins(t *testing.T) {
	dir := t.TempDir()
	owners := []routefile.Owner{
		{Dir: dir, File: "devingress.toml", Domain: "example.dev", Remote: true, Sites: []routefile.Site{{Host: "web.example.dev"}, {Host: "a.b.example.dev"}}},
		{Dir: dir, Name: "nodomain", File: "x.toml", Remote: true, Sites: []routefile.Site{{Host: "web"}}},
		{Dir: dir, Name: "local", File: "y.toml", Domain: "example.dev", Sites: []routefile.Site{{Host: "c.d.example.dev"}}},
		{Dir: filepath.Join(dir, "gone"), Domain: "example.dev", Remote: true, Sites: []routefile.Site{{Host: "e.f.example.dev"}}},
	}
	got := checkTwins(owners, suffix)
	if len(got) != 2 || got[0].Subject != "a.b.example.dev" || got[1].Subject != "web" || got[0].Status != Warn ||
		!strings.Contains(got[1].Message, "sets no domain") {
		t.Errorf("checkTwins = %+v", got)
	}
}

// Run in remote mode, with every outward dependency faked: each remote check appears, the
// end-to-end check dials the tailnet address, and no output holds the token.
func TestRunRemote(t *testing.T) {
	root := isolate(t)
	t.Setenv("CAROOT", t.TempDir())
	confDir := filepath.Join(root, "xdg_config_home", "devingress")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "cloudflare-token"), []byte(fakeToken+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	conf := "https_port = 18443\ndns_listen = \"\"\n[remote]\nsuffix = \"" + suffix + "\"\ntoken_file = \"cloudflare-token\"\n"
	if err := os.WriteFile(filepath.Join(confDir, "config.toml"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}

	addr, pool := wildcardServer(t, time.Now().Add(60*24*time.Hour))
	var asked []string
	deps := certDeps(addr, pool, time.Now(), &asked)
	deps.GOOS = "linux"
	deps.Dial = func(string, string, time.Duration) (net.Conn, error) { return nil, errors.New("refused") }
	deps.Lookup = func(string) ([]net.IP, error) { return []net.IP{tailnetIP.AsSlice()}, nil }
	deps.Interfaces = func() ([]Iface, error) {
		return []Iface{{Name: "lo", Addrs: []net.IP{net.ParseIP("127.0.0.1")}}, {Name: "tailscale0", Addrs: []net.IP{tailnetIP.AsSlice()}}}, nil
	}
	deps.ReadFile = os.ReadFile
	deps.Glob = func(string) ([]string, error) { return nil, nil }
	deps.Executable = func() (string, error) { return "/usr/local/bin/devingress", nil }
	deps.TailscaleServeStatus = fixture(t, "serve-status-forward.json")
	deps.HTTPDo = verifyAPI(t, 200, `{"success":true,"result":{"status":"active"}}`)

	checks := Run(deps)
	var got []string
	for _, c := range checks {
		got = append(got, string(c.Status)+" "+c.Check+" "+c.Subject)
	}
	tokenPath := filepath.Join(confDir, "cloudflare-token")
	for _, want := range []string{
		"ok tailscale tailscale serve, tailnet port 443",
		"ok tailnet tailnet address",
		"ok wildcard *." + suffix + " DNS",
		"ok certificate *." + suffix + " at 127.0.0.1:18443",
		"ok end to end *." + suffix + " at 100.64.0.10:443",
		"warn token " + tokenPath, // mode 644
		"ok token " + tokenPath,   // active
	} {
		if !slices.Contains(got, want) {
			t.Errorf("no check %q in:\n%s", want, strings.Join(got, "\n"))
		}
	}
	out, err := json.Marshal(checks)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), fakeToken) {
		t.Fatalf("doctor output quotes the token:\n%s", out)
	}
	if len(asked) != 2 || !strings.HasPrefix(asked[1], "100.64.0.10:443 ") {
		t.Errorf("TLS dials = %q, want loopback then the tailnet address", asked)
	}
}
