package render

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"

	"github.com/WonderPanda/devingress/internal/routefile"
)

func opts(t *testing.T) Options {
	return Options{
		AdminSocket: t.TempDir() + "/admin.sock",
		StorageDir:  t.TempDir(),
		Bind:        []string{"127.0.0.1"},
		HTTPPort:    18080,
		HTTPSPort:   18443,
	}
}

func TestCaddyfileAdaptsEluveShapedRoutes(t *testing.T) {
	owners := []routefile.Owner{{
		Dir: "/w/eluve-nx.eng-1",
		Sites: []routefile.Site{
			{Host: "web-eng-1.eluve.dev", Upstream: "127.0.0.1:10299", Retry: "5s"},
			{Host: "medplum-eng-1.eluve.dev", Upstream: "127.0.0.1:10301", Protocols: []string{"h1"}, Caddy: `@options method OPTIONS
handle @options {
	header Access-Control-Allow-Origin "{http.request.header.Origin}"
	respond 204
}
reverse_proxy 127.0.0.1:10301 {
	header_down -Access-Control-Allow-Origin
}`},
		},
		Paths: []routefile.PathRoute{{Prefix: "/supertokens-eng-1", Upstream: "127.0.0.1:10296", Strip: true, Proxy: `header_down Cache-Control "max-age=45, must-revalidate"`}},
	}, {
		Dir:   "/w/orchard.note-findings",
		Sites: []routefile.Site{{Host: "orchard-note-findings.eluve.dev", Upstream: "127.0.0.1:11601", Retry: "5s"}},
	}}

	text, err := Caddyfile(opts(t), owners)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"alpn http/1.1", "lb_try_duration 5s", "handle_path /supertokens-eng-1/*", "http://:18080 {"} {
		if !strings.Contains(text, want) {
			t.Errorf("Caddyfile missing %q:\n%s", want, text)
		}
	}
	cfg, err := Adapt(text)
	if err != nil {
		t.Fatalf("adapt: %v\n%s", err, text)
	}
	if !strings.Contains(string(cfg), "orchard-note-findings.eluve.dev") {
		t.Errorf("adapted JSON lost a host")
	}
}

// macOS keeps mkcert's root in ~/Library/Application Support, and a home dir
// can hold a space too: every path must stay one token.
func TestCaddyfilePathsWithSpaces(t *testing.T) {
	dir := t.TempDir() + "/Application Support"
	o := opts(t)
	o.AdminSocket = dir + "/admin.sock"
	o.StorageDir = dir + "/caddy"
	o.CARoot = dir + "/mkcert"
	writeCA(t, o.CARoot)
	text, err := Caddyfile(o, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Adapt(text)
	if err != nil {
		t.Fatalf("adapt: %v\n%s", err, text)
	}
	for _, want := range []string{"unix/" + o.AdminSocket, o.StorageDir, o.CARoot + "/rootCA.pem", o.CARoot + "/rootCA-key.pem"} {
		w, _ := json.Marshal(want)
		if !strings.Contains(string(cfg), string(w)) {
			t.Errorf("adapted JSON missing %s:\n%s", w, cfg)
		}
	}
}

// writeCA writes a throwaway root, rootCA.pem and rootCA-key.pem, to dir: Adapt
// provisions the pki app, which loads them.
func writeCA(t *testing.T, dir string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "devingress render test root"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/rootCA.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/rootCA-key.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCaddyfileRejectsConflicts(t *testing.T) {
	site := routefile.Site{Host: "web-x.eluve.dev", Upstream: "127.0.0.1:1"}
	_, err := Caddyfile(opts(t), []routefile.Owner{
		{Dir: "/a", Sites: []routefile.Site{site}},
		{Dir: "/b", Sites: []routefile.Site{site}},
	})
	if err == nil || !strings.Contains(err.Error(), "/a and /b") {
		t.Fatalf("err = %v, want a conflict naming both owners", err)
	}
}

func TestCaddyfileNamedOwners(t *testing.T) {
	text, err := Caddyfile(opts(t), []routefile.Owner{
		{Dir: "/repo", Sites: []routefile.Site{{Host: "main.eluve.dev", Upstream: "127.0.0.1:1"}}},
		{Dir: "/repo", Name: "coding", Sites: []routefile.Site{{Host: "billing-coding.eluve.dev", Upstream: "127.0.0.1:2"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# owner: /repo\n", "# owner: /repo [coding]\n", "(owner /repo [coding])"} {
		if !strings.Contains(text, want) {
			t.Errorf("Caddyfile missing %q:\n%s", want, text)
		}
	}

	site := routefile.Site{Host: "x.eluve.dev", Upstream: "127.0.0.1:1"}
	_, err = Caddyfile(opts(t), []routefile.Owner{
		{Dir: "/repo", Sites: []routefile.Site{site}},
		{Dir: "/repo", Name: "coding", Sites: []routefile.Site{site}},
	})
	if err == nil || err.Error() != "host x.eluve.dev is claimed by both /repo and /repo [coding]" {
		t.Fatalf("err = %v, want a conflict naming both owners", err)
	}
}

func TestAdaptRejectsInvalidRawCaddy(t *testing.T) {
	text, err := Caddyfile(opts(t), []routefile.Owner{{
		Dir:   "/a",
		Sites: []routefile.Site{{Host: "x.eluve.dev", Caddy: "not_a_directive foo"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Adapt(text); err == nil {
		t.Fatal("want an unknown directive to fail validation")
	}
}

func TestCaddyfileSiteNamesUnreachableUpstream(t *testing.T) {
	text, err := Caddyfile(opts(t), []routefile.Owner{{
		Dir:   "/w/orchard.updates",
		Sites: []routefile.Site{{Host: "orchard-updates.eluve.dev", Upstream: "127.0.0.1:15766", Retry: "5s"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := "\thandle_errors 502 503 504 {\n" +
		"\t\theader Content-Type \"text/plain; charset=utf-8\"\n" +
		"\t\trespond \"devingress: nothing answered at 127.0.0.1:15766 for orchard-updates.eluve.dev (owner /w/orchard.updates). " +
		"Start its dev server, or run \\\"devingress status\\\".\" {err.status_code}\n" +
		"\t}\n"
	if !strings.Contains(text, want) {
		t.Errorf("Caddyfile missing\n%s\nin:\n%s", want, text)
	}
	if _, err := Adapt(text); err != nil {
		t.Fatalf("adapt: %v\n%s", err, text)
	}
}

func TestCaddyfilePathsNameUnreachableUpstream(t *testing.T) {
	text, err := Caddyfile(opts(t), []routefile.Owner{
		{Dir: "/w/a", Paths: []routefile.PathRoute{{Prefix: "/auth-a", Upstream: "127.0.0.1:10296", Strip: true}}},
		{Dir: "/w/b", Paths: []routefile.PathRoute{{Prefix: "/api-b", Upstream: "127.0.0.1:10297"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(text, "handle_errors"); n != 1 {
		t.Errorf("%d handle_errors blocks, want 1:\n%s", n, text)
	}
	_, errs, _ := strings.Cut(text, "handle_errors 502 503 504 {\n")
	for _, want := range []string{
		"\t\thandle /auth-a/* {\n\t\t\theader Content-Type \"text/plain; charset=utf-8\"\n" +
			"\t\t\trespond \"devingress: nothing answered at 127.0.0.1:10296 for /auth-a/* (owner /w/a).",
		"\t\thandle /api-b/* {\n\t\t\theader Content-Type \"text/plain; charset=utf-8\"\n" +
			"\t\t\trespond \"devingress: nothing answered at 127.0.0.1:10297 for /api-b/* (owner /w/b).",
	} {
		if !strings.Contains(errs, want) {
			t.Errorf("handle_errors missing\n%s\nin:\n%s", want, text)
		}
	}
	if _, err := Adapt(text); err != nil {
		t.Fatalf("adapt: %v\n%s", err, text)
	}
}

func TestCaddyfileLeavesRawCaddyAlone(t *testing.T) {
	body := "reverse_proxy 127.0.0.1:10301 {\n\theader_down -Access-Control-Allow-Origin\n}\n"
	text, err := Caddyfile(opts(t), []routefile.Owner{{
		Dir:   "/w/a",
		Sites: []routefile.Site{{Host: "medplum.eluve.dev", Upstream: "127.0.0.1:10301", Caddy: body}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "medplum.eluve.dev {\n"+body+"}\n") || strings.Contains(text, "handle_errors") {
		t.Errorf("raw caddy body changed:\n%s", text)
	}
	if _, err := Adapt(text); err != nil {
		t.Fatalf("adapt: %v\n%s", err, text)
	}
}

// The owner dir lands in a quoted token that Caddy placeholder-expands per
// request, so a quote or a known placeholder name must come out as written.
func TestCaddyfileUnreachableBodyIsLiteral(t *testing.T) {
	dir := `/w/a "b" {path}`
	text, err := Caddyfile(opts(t), []routefile.Owner{{
		Dir:   dir,
		Sites: []routefile.Site{{Host: "x.eluve.dev", Upstream: "127.0.0.1:1"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Adapt(text)
	if err != nil {
		t.Fatalf("adapt: %v\n%s", err, text)
	}
	var tree any
	if err := json.Unmarshal(cfg, &tree); err != nil {
		t.Fatal(err)
	}
	bodies := staticBodies(tree)
	if len(bodies) != 1 {
		t.Fatalf("%d static_response bodies, want 1: %q", len(bodies), bodies)
	}
	repl := caddy.NewEmptyReplacer()
	repl.Set("path", "/requested")
	got := repl.ReplaceAll(bodies[0], "")
	want := `devingress: nothing answered at 127.0.0.1:1 for x.eluve.dev (owner ` + dir + `). Start its dev server, or run "devingress status".`
	if got != want {
		t.Errorf("served body\n%s\nwant\n%s", got, want)
	}
}

func staticBodies(v any) []string {
	var out []string
	switch v := v.(type) {
	case map[string]any:
		if v["handler"] == "static_response" {
			if body, ok := v["body"].(string); ok {
				out = append(out, body)
			}
		}
		for _, c := range v {
			out = append(out, staticBodies(c)...)
		}
	case []any:
		for _, c := range v {
			out = append(out, staticBodies(c)...)
		}
	}
	return out
}
