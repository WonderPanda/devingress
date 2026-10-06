package render

import (
	"strings"
	"testing"

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
