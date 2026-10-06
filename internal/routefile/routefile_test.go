package routefile

import (
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
		"porch.toml": `
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
			dir := write(t, map[string]string{"porch.toml": c.toml})
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
