package render

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/WonderPanda/devingress/internal/routefile"
)

var update = flag.Bool("update", false, "rewrite testdata/*.Caddyfile from the current output")

// golden compares got with testdata/name, or rewrites it under -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("Caddyfile differs from %s (go test -run %s -update rewrites it):\n%s", path, t.Name(), got)
	}
}

// goldenOwners loads testdata/routes.toml the way "devingress up --slug my-branch" does.
func goldenOwners(t *testing.T) []routefile.Owner {
	t.Helper()
	abs, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatal(err)
	}
	o, err := routefile.Load(abs, "routes.toml", "my-branch")
	if err != nil {
		t.Fatal(err)
	}
	o.Dir = "/src/app"
	return []routefile.Owner{o}
}

func goldenOptions() Options {
	return Options{
		AdminSocket: "/run/user/1000/devingress/admin.sock",
		StorageDir:  "/home/me/.local/share/devingress/caddy",
		CARoot:      "/home/me/.local/share/mkcert",
		Bind:        []string{"127.0.0.1", "172.17.0.1"},
		HTTPPort:    80,
		HTTPSPort:   443,
	}
}

// Without [remote], the Caddyfile is what it was before remote mode existed.
func TestGoldenLocalOnly(t *testing.T) {
	text, err := Caddyfile(goldenOptions(), goldenOwners(t))
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "local.Caddyfile", text)
}

const goldenSuffix = "machine.example.dev"

// remoteOwners is goldenOwners opted in with remote = true, plus an owner that is not opted in
// and one whose host needs two labels under the suffix.
func remoteOwners(t *testing.T) []routefile.Owner {
	owners := goldenOwners(t)
	owners[0].Remote = true
	return append(owners,
		routefile.Owner{Dir: "/src/docs", Domain: "example.dev", Sites: []routefile.Site{{Host: "docs.example.dev", Upstream: "localhost:4000"}}},
		routefile.Owner{Dir: "/src/deep", Domain: "example.dev", Remote: true, Sites: []routefile.Site{{Host: "a.b.example.dev", Upstream: "localhost:4001"}}},
	)
}

func TestGoldenRemote(t *testing.T) {
	o := goldenOptions()
	o.Remote = &Remote{Suffix: goldenSuffix, TokenFile: "/home/me/.config/devingress/cloudflare-token"}
	text, err := Caddyfile(o, remoteOwners(t))
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "remote.Caddyfile", text)
}
