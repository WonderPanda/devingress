package render

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"

	"github.com/WonderPanda/devingress/internal/routefile"
)

// fakeToken matches caddy-dns/cloudflare's format check; it is no real token.
const fakeToken = "cfut_" + "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN"

func writeToken(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cloudflare-token")
	if err := os.WriteFile(path, []byte(fakeToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// tlsApp is the part of the adapted JSON's tls app the remote tests read.
type tlsApp struct {
	Automation struct {
		Policies []policy `json:"policies"`
	} `json:"automation"`
	Certificates json.RawMessage `json:"certificates"`
}

type policy struct {
	Subjects []string `json:"subjects"`
	Issuers  []struct {
		Module     string `json:"module"`
		Challenges struct {
			DNS struct {
				Provider struct {
					Name     string `json:"name"`
					APIToken string `json:"api_token"`
				} `json:"provider"`
			} `json:"dns"`
		} `json:"challenges"`
	} `json:"issuers"`
}

func (p policy) modules() []string {
	var out []string
	for _, i := range p.Issuers {
		out = append(out, i.Module)
	}
	return out
}

// matchWildcard is certmagic.MatchWildcard: name equals subject, or subject is *.<rest> and
// name is one label in front of rest.
func matchWildcard(name, subject string) bool {
	if name == subject {
		return true
	}
	rest, ok := strings.CutPrefix(subject, "*.")
	if !ok {
		return false
	}
	_, nameRest, ok := strings.Cut(name, ".")
	return ok && nameRest == rest
}

// policyFor is caddytls's getAutomationPolicyForName: the first policy with no subjects or a
// subject that matches name.
func policyFor(policies []policy, name string) (policy, bool) {
	for _, p := range policies {
		if len(p.Subjects) == 0 || slices.ContainsFunc(p.Subjects, func(s string) bool { return matchWildcard(name, s) }) {
			return p, true
		}
	}
	return policy{}, false
}

func remoteText(t *testing.T, tokenFile string) (Options, string) {
	t.Helper()
	o := opts(t)
	o.CARoot = filepath.Join(t.TempDir(), "ca")
	writeCA(t, o.CARoot)
	o.Remote = &Remote{Suffix: goldenSuffix, TokenFile: tokenFile}
	text, err := Caddyfile(o, remoteOwners(t))
	if err != nil {
		t.Fatal(err)
	}
	return o, text
}

// The adapted JSON gets exactly one public certificate, *.<suffix> through Cloudflare DNS-01,
// and Caddy's policy lookup sends every remote name to it, never to the local CA.
func TestRemoteAdaptedJSON(t *testing.T) {
	tokenFile := writeToken(t)
	_, text := remoteText(t, tokenFile)
	cfgJSON, err := Adapt(text)
	if err != nil {
		t.Fatalf("adapt: %v\n%s", err, text)
	}
	if strings.Contains(text, fakeToken) || strings.Contains(string(cfgJSON), fakeToken) {
		t.Fatal("the token value is in the Caddyfile or the JSON")
	}
	var cfg struct {
		Apps struct {
			TLS  tlsApp `json:"tls"`
			HTTP struct {
				Servers map[string]struct {
					StrictSNIHost       *bool `json:"strict_sni_host"`
					TLSConnectionPolicy []struct {
						Match struct {
							SNI []string `json:"sni"`
						} `json:"match"`
						ALPN []string `json:"alpn"`
					} `json:"tls_connection_policies"`
				} `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(cfgJSON, &cfg); err != nil {
		t.Fatal(err)
	}
	policies := cfg.Apps.TLS.Automation.Policies

	var acmeSubjects []string
	for _, p := range policies {
		for _, m := range p.modules() {
			if m != "internal" {
				acmeSubjects = append(acmeSubjects, p.Subjects...)
			}
			if m != "internal" && m != "acme" {
				t.Errorf("issuer %q in policy %+v: want only acme (the wildcard) and internal (local hosts)", m, p)
			}
		}
	}
	if want := []string{"*." + goldenSuffix}; !slices.Equal(acmeSubjects, want) {
		t.Errorf("ACME subjects = %q, want %q", acmeSubjects, want)
	}

	remoteNames := []string{"*." + goldenSuffix, "web-my-branch." + goldenSuffix, "realtime-my-branch." + goldenSuffix, "api-my-branch." + goldenSuffix}
	for _, name := range remoteNames {
		p, ok := policyFor(policies, name)
		if !ok || !slices.Equal(p.modules(), []string{"acme"}) {
			t.Errorf("%s: policy %+v, want the acme wildcard policy", name, p)
			continue
		}
		prov := p.Issuers[0].Challenges.DNS.Provider
		if prov.Name != "cloudflare" || prov.APIToken != "{file."+tokenFile+"}" {
			t.Errorf("%s: DNS provider %+v, want cloudflare with the {file.%s} placeholder", name, prov, tokenFile)
		}
		for _, q := range policies {
			if slices.Contains(q.modules(), "internal") && slices.ContainsFunc(q.Subjects, func(s string) bool { return matchWildcard(name, s) }) {
				t.Errorf("%s is a subject of internal-issuer policy %+v", name, q)
			}
		}
	}
	for _, name := range []string{"web-my-branch.example.dev", "realtime-my-branch.example.dev", "docs.example.dev"} {
		if p, ok := policyFor(policies, name); !ok || !slices.Equal(p.modules(), []string{"internal"}) {
			t.Errorf("local %s: policy %+v, want the local CA", name, p)
		}
	}
	if len(cfg.Apps.TLS.Certificates) != 0 {
		t.Errorf("tls app loads certificates: %s", cfg.Apps.TLS.Certificates)
	}

	var strict, pinned bool
	for _, srv := range cfg.Apps.HTTP.Servers {
		strict = strict || (srv.StrictSNIHost != nil && *srv.StrictSNIHost)
		for _, cp := range srv.TLSConnectionPolicy {
			if slices.Contains(cp.Match.SNI, "realtime-my-branch."+goldenSuffix) && slices.Equal(cp.ALPN, []string{"http/1.1"}) {
				pinned = true
			}
		}
	}
	if !strict {
		t.Error("no server sets strict_sni_host")
	}
	if !pinned {
		t.Error("the protocols = [\"h1\"] twin is not pinned to http/1.1")
	}
}

func TestRemoteTwinConflicts(t *testing.T) {
	o := opts(t)
	o.Remote = &Remote{Suffix: goldenSuffix, TokenFile: "/t"}
	_, err := Caddyfile(o, []routefile.Owner{
		{Dir: "/a", Domain: "example.dev", Remote: true, Sites: []routefile.Site{{Host: "web.example.dev", Upstream: "localhost:1"}}},
		{Dir: "/b", Domain: "other.dev", Remote: true, Sites: []routefile.Site{{Host: "web.other.dev", Upstream: "localhost:2"}}},
	})
	if err == nil || err.Error() != "host web.machine.example.dev is claimed by both /a and /b" {
		t.Fatalf("err = %v, want the twins' conflict", err)
	}
	// Without remote mode the same owners do not conflict.
	o.Remote = nil
	if _, err := Caddyfile(o, []routefile.Owner{
		{Dir: "/a", Domain: "example.dev", Remote: true, Sites: []routefile.Site{{Host: "web.example.dev", Upstream: "localhost:1"}}},
		{Dir: "/b", Domain: "other.dev", Remote: true, Sites: []routefile.Site{{Host: "web.other.dev", Upstream: "localhost:2"}}},
	}); err != nil {
		t.Fatal(err)
	}
}

// runCaddy loads cfgJSON, with its admin endpoint off and nothing persisted, into this
// process's Caddy, and stops it when the test ends.
func runCaddy(t *testing.T, cfgJSON []byte) {
	t.Helper()
	var cfg map[string]any
	if err := json.Unmarshal(cfgJSON, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["admin"] = map[string]any{"disabled": true, "config": map[string]any{"persist": false}}
	cfg["logging"] = map[string]any{"logs": map[string]any{"default": map[string]any{"level": "ERROR"}}}
	out, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := caddy.Load(out, true); err != nil {
		t.Fatalf("load: %v", err)
	}
	t.Cleanup(func() { _ = caddy.Stop() })
}

// setWildcardIssuer replaces the issuers of the *.<suffix> policy in cfgJSON.
func setWildcardIssuer(t *testing.T, cfgJSON []byte, issuer map[string]any) []byte {
	t.Helper()
	var cfg map[string]any
	if err := json.Unmarshal(cfgJSON, &cfg); err != nil {
		t.Fatal(err)
	}
	policies := cfg["apps"].(map[string]any)["tls"].(map[string]any)["automation"].(map[string]any)["policies"].([]any)
	found := false
	for _, p := range policies {
		p := p.(map[string]any)
		if subj, _ := p["subjects"].([]any); len(subj) == 1 && subj[0] == "*."+goldenSuffix {
			p["issuers"] = []any{issuer}
			found = true
		}
	}
	if !found {
		t.Fatalf("no *.%s policy", goldenSuffix)
	}
	out, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// remoteServer renders remoteOwners with every upstream pointed at a test server, on free
// ports, and returns the options, the adapted JSON and the local root's pool.
func remoteServer(t *testing.T) (Options, []byte, *x509.CertPool) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "upstream saw %s", r.Host)
	}))
	t.Cleanup(up.Close)
	addr := strings.TrimPrefix(up.URL, "http://")

	o := opts(t)
	o.HTTPPort, o.HTTPSPort = freeTCPPort(t), freeTCPPort(t)
	o.CARoot = filepath.Join(t.TempDir(), "ca")
	writeCA(t, o.CARoot)
	o.Remote = &Remote{Suffix: goldenSuffix, TokenFile: writeToken(t)}
	owners := []routefile.Owner{{Dir: "/src/app", Domain: "example.dev", Remote: true, Sites: []routefile.Site{
		{Host: "web.example.dev", Upstream: addr},
		{Host: "realtime.example.dev", Upstream: addr, Protocols: []string{"h1"}},
	}}}
	text, err := Caddyfile(o, owners)
	if err != nil {
		t.Fatal(err)
	}
	cfgJSON, err := Adapt(text)
	if err != nil {
		t.Fatalf("adapt: %v\n%s", err, text)
	}
	roots := x509.NewCertPool()
	pemData, err := os.ReadFile(filepath.Join(o.CARoot, "rootCA.pem"))
	if err != nil {
		t.Fatal(err)
	}
	roots.AppendCertsFromPEM(pemData)
	return o, cfgJSON, roots
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// The wildcard certificate cannot be obtained (its ACME CA refuses connections): a twin's
// handshake then fails rather than falling back to a local-CA certificate, while local hosts
// keep theirs. Nothing leaves this machine.
func TestRemoteTwinNeverGetsLocalCert(t *testing.T) {
	o, cfgJSON, roots := remoteServer(t)
	dead := "http://127.0.0.1:1/directory"
	cfgJSON = setWildcardIssuer(t, cfgJSON, map[string]any{
		"module": "acme", "ca": dead, "test_ca": dead,
		"challenges": map[string]any{"dns": map[string]any{"provider": map[string]any{"name": "cloudflare", "api_token": "{file." + o.Remote.TokenFile + "}"}}},
	})
	runCaddy(t, cfgJSON)
	addr := fmt.Sprintf("127.0.0.1:%d", o.HTTPSPort)

	waitHandshake(t, addr, "web.example.dev", roots)
	for _, sni := range []string{"web." + goldenSuffix, "realtime." + goldenSuffix, "unknown." + goldenSuffix} {
		conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: sni, InsecureSkipVerify: true})
		if err == nil {
			cert := conn.ConnectionState().PeerCertificates[0]
			conn.Close()
			t.Errorf("SNI %s: served a certificate for %q issued by %q, want no certificate until the wildcard is issued",
				sni, cert.DNSNames, cert.Issuer.CommonName)
		}
	}
}

func waitHandshake(t *testing.T, addr, sni string, roots *x509.CertPool) *x509.Certificate {
	t.Helper()
	var err error
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		var conn *tls.Conn
		if conn, err = tls.Dial("tcp", addr, &tls.Config{ServerName: sni, RootCAs: roots}); err == nil {
			defer conn.Close()
			return conn.ConnectionState().PeerCertificates[0]
		}
	}
	t.Fatalf("handshake with SNI %s: %v", sni, err)
	return nil
}

// With the wildcard certificate in place (issued here by the local CA, standing in for Let's
// Encrypt): every twin is served that one certificate, a request whose Host differs from the
// connection's SNI gets 421 so the browser reconnects (and an h1-pinned twin negotiates h1),
// and local hosts still answer 200.
func TestRemoteStrictSNI(t *testing.T) {
	o, cfgJSON, roots := remoteServer(t)
	runCaddy(t, setWildcardIssuer(t, cfgJSON, map[string]any{"module": "internal"}))
	addr := fmt.Sprintf("127.0.0.1:%d", o.HTTPSPort)

	for _, sni := range []string{"web." + goldenSuffix, "realtime." + goldenSuffix} {
		cert := waitHandshake(t, addr, sni, roots)
		if !slices.Equal(cert.DNSNames, []string{"*." + goldenSuffix}) {
			t.Errorf("SNI %s: certificate for %q, want the one wildcard", sni, cert.DNSNames)
		}
	}
	waitHandshake(t, addr, "web.example.dev", roots)

	for _, c := range []struct {
		sni, host string
		status    int
		proto     int
	}{
		{"web." + goldenSuffix, "realtime." + goldenSuffix, http.StatusMisdirectedRequest, 2},
		{"web." + goldenSuffix, "web." + goldenSuffix, http.StatusOK, 2},
		{"realtime." + goldenSuffix, "realtime." + goldenSuffix, http.StatusOK, 1},
		{"web.example.dev", "web.example.dev", http.StatusOK, 2},
	} {
		resp, body := request(t, addr, roots, c.sni, c.host)
		if resp.StatusCode != c.status || resp.ProtoMajor != c.proto {
			t.Errorf("SNI %s, Host %s: %s over HTTP/%d (%q), want %d over HTTP/%d", c.sni, c.host, resp.Status, resp.ProtoMajor, body, c.status, c.proto)
		}
	}
}

// request sends GET https://<host>/ over a new connection to addr whose TLS SNI is sni.
func request(t *testing.T, addr string, roots *x509.CertPool, sni, host string) (*http.Response, string) {
	t.Helper()
	tr := &http.Transport{
		TLSClientConfig:   &tls.Config{ServerName: sni, RootCAs: roots},
		ForceAttemptHTTP2: true,
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("tcp", addr)
		},
	}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequest(http.MethodGet, "https://"+sni+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	resp, err := (&http.Client{Transport: tr, Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("SNI %s, Host %s: %v", sni, host, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}
