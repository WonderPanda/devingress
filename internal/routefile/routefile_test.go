package routefile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadResolvesTemplates(t *testing.T) {
	dir := write(t, map[string]string{
		".env.worktree": "VITE_PORT=10299\nexport MEDPLUM_PORT='10301'\n# ENABLE_MEDPLUM=false\nENABLE_MEDPLUM=true\n",
		"devingress.toml": `
domain = "eluve.dev"
env = [".env.worktree"]

[[route]]
host = "web-${slug}"
to = "${VITE_PORT}"
retry = "5s"

[[route]]
host = "medplum-${slug}"
to = "${MEDPLUM_PORT}"
when = "${ENABLE_MEDPLUM}"
protocols = ["h1"]
caddy = '''
header Access-Control-Allow-Origin "{http.request.header.Origin}"
reverse_proxy ${upstream}
'''

[[route]]
host = "off-${slug}"
to = "1"
when = "${NOT_SET_ANYWHERE}"

[[path]]
prefix = "/supertokens-${slug}/"
to = "host.docker.internal:3567"
strip = true
`,
	})

	o, err := Load(dir, DefaultName, "eng-1-thing")
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Sites) != 2 {
		t.Fatalf("want the when-gated route skipped, got %d sites: %+v", len(o.Sites), o.Sites)
	}
	web, med := o.Sites[0], o.Sites[1]
	if web.Host != "web-eng-1-thing.eluve.dev" || web.Upstream != "127.0.0.1:10299" || web.Retry != "5s" {
		t.Errorf("web site = %+v", web)
	}
	if med.Upstream != "127.0.0.1:10301" {
		t.Errorf("quoted env value not unquoted: %q", med.Upstream)
	}
	if !strings.Contains(med.Caddy, "{http.request.header.Origin}") || !strings.Contains(med.Caddy, "reverse_proxy 127.0.0.1:10301") {
		t.Errorf("raw caddy should keep Caddy placeholders and expand ${upstream}:\n%s", med.Caddy)
	}
	if len(o.Paths) != 1 || o.Paths[0].Prefix != "/supertokens-eng-1-thing" || o.Paths[0].Upstream != "host.docker.internal:3567" {
		t.Errorf("path = %+v", o.Paths)
	}
}

func TestLoadErrors(t *testing.T) {
	cases := map[string]struct{ toml, want string }{
		"undefined variable": {`[[route]]
host = "web"
to = "${NOPE_NOT_DEFINED}"`, "NOPE_NOT_DEFINED"},
		"caddy with retry": {`[[route]]
host = "web"
to = "1"
retry = "5s"
caddy = "respond 200"`, "cannot be combined"},
		"missing host": {`[[route]]
to = "1"`, "host"},
		"relative prefix": {`[[path]]
prefix = "nope"
to = "1"`, "must start with /"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := write(t, map[string]string{"devingress.toml": c.toml})
			_, err := Load(dir, DefaultName, "s")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"jesse/ENG-123_Fix the thing": "jesse-eng-123-fix-the-thing",
		"--main--":                    "main",
	} {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadPaths(t *testing.T) {
	const toml = `env = [%q]

[[route]]
host = "web"
to = "${PORT}"
`
	route := func(env string) string { return fmt.Sprintf(toml, env) }
	port := func(t *testing.T, o Owner, want string) {
		t.Helper()
		if len(o.Sites) != 1 || o.Sites[0].Upstream != "127.0.0.1:"+want {
			t.Errorf("sites = %+v, want upstream 127.0.0.1:%s", o.Sites, want)
		}
	}

	t.Run("absolute route file and env outside dir", func(t *testing.T) {
		dir := write(t, nil)
		elsewhere := write(t, map[string]string{".env": "PORT=1001\n"})
		other := write(t, map[string]string{"devingress.toml": route(filepath.Join(elsewhere, ".env"))})
		name := filepath.Join(other, "devingress.toml")
		o, err := Load(dir, name, "s")
		if err != nil {
			t.Fatal(err)
		}
		if o.File != name || o.Dir != dir {
			t.Errorf("File = %q, Dir = %q; want %q, %q", o.File, o.Dir, name, dir)
		}
		port(t, o, "1001")
	})

	t.Run("absolute env outside dir", func(t *testing.T) {
		elsewhere := write(t, map[string]string{".env": "PORT=1004\n"})
		dir := write(t, map[string]string{"devingress.toml": route(filepath.Join(elsewhere, ".env"))})
		o, err := Load(dir, DefaultName, "s")
		if err != nil {
			t.Fatal(err)
		}
		port(t, o, "1004")
	})

	t.Run("relative route file resolves against dir", func(t *testing.T) {
		dir := write(t, map[string]string{".env": "PORT=1002\n"})
		other := write(t, map[string]string{"devingress.toml": route(".env")})
		rel, err := filepath.Rel(dir, filepath.Join(other, "devingress.toml"))
		if err != nil {
			t.Fatal(err)
		}
		o, err := Load(dir, rel, "s")
		if err != nil {
			t.Fatal(err)
		}
		if o.File != rel {
			t.Errorf("File = %q, want %q", o.File, rel)
		}
		port(t, o, "1002")
	})

	t.Run("relative env resolves against dir when the route file is elsewhere", func(t *testing.T) {
		dir := write(t, map[string]string{".env": "PORT=1003\n"})
		other := write(t, map[string]string{"devingress.toml": route(".env"), ".env": "PORT=9999\n"})
		o, err := Load(dir, filepath.Join(other, "devingress.toml"), "s")
		if err != nil {
			t.Fatal(err)
		}
		port(t, o, "1003")
	})
}
