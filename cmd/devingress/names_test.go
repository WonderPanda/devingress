package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/WonderPanda/devingress/internal/routefile"
	"github.com/WonderPanda/devingress/internal/state"
)

// namedCheckout makes an isolated checkout with two route files: devingress.toml claims
// main.example.dev, devingress.coding.toml coding.example.dev.
func namedCheckout(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(isolate(t))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "repo")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, host := range map[string]string{"devingress.toml": "main", "devingress.coding.toml": "coding"} {
		toml := "domain = \"example.dev\"\n\n[[route]]\nhost = \"" + host + "\"\nto = \"3000\"\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func owners(t *testing.T) []routefile.Owner {
	t.Helper()
	list, err := state.New(filepath.Join(os.Getenv("XDG_STATE_HOME"), "devingress")).List()
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func labels(list []routefile.Owner) []string {
	var out []string
	for _, o := range list {
		out = append(out, o.Label())
	}
	return out
}

func cli(t *testing.T, args ...string) string {
	t.Helper()
	code, stdout, stderr := runCLI(t, args...)
	if code != 0 {
		t.Fatalf("%v: exit %d, stderr %q", args, code, stderr)
	}
	return stdout
}

func upMain(t *testing.T, dir string) { cli(t, "up", dir, "--slug", "x") }
func upCoding(t *testing.T, dir string) {
	cli(t, "up", dir, "--slug", "x", "--name", "coding", "--file", "devingress.coding.toml")
}

func TestNamedOwnersCoexist(t *testing.T) {
	dir := namedCheckout(t)
	pinHome(t)
	upMain(t, dir)
	if out := cli(t, "up", dir, "--slug", "x", "--name", "coding", "--file", "devingress.coding.toml"); !strings.Contains(out, "https://coding.example.dev") {
		t.Errorf("up --name coding printed:\n%s", out)
	}
	if got, want := labels(owners(t)), []string{dir, dir + " [coding]"}; !slices.Equal(got, want) {
		t.Fatalf("owners = %q, want %q", got, want)
	}

	caddy := cli(t, "caddyfile")
	for _, want := range []string{"# owner: " + dir + "\n", "# owner: " + dir + " [coding]\n", "main.example.dev {", "coding.example.dev {",
		"(owner " + dir + " [coding])"} {
		if !strings.Contains(caddy, want) {
			t.Errorf("caddyfile missing %q:\n%s", want, caddy)
		}
	}
	if status := cli(t, "status"); !strings.Contains(status, "\n"+dir+" [coding]  (devingress.coding.toml, slug x)\n") ||
		!strings.Contains(status, "\n"+dir+"  (devingress.toml, slug x)\n") {
		t.Errorf("status:\n%s", status)
	}
	want := []string{dir, "  https://main.example.dev  127.0.0.1:3000", "", dir + " [coding]", "  https://coding.example.dev  127.0.0.1:3000"}
	if got := routesText(t); !slices.Equal(got, want) {
		t.Errorf("routes:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	routes := routesJSON(t, 0, "--dir", dir)
	if len(routes) != 2 || *routes[0].Host != "coding.example.dev" || routes[0].Name == nil || *routes[0].Name != "coding" ||
		*routes[1].Host != "main.example.dev" || routes[1].Name != nil {
		t.Errorf("routes --json: %+v", routes)
	}
}

func TestDownNamed(t *testing.T) {
	dir := namedCheckout(t)
	upMain(t, dir)
	upCoding(t, dir)
	if out := cli(t, "down", dir, "--name", "coding"); out != "  devingress is not serving; routes apply when `devingress serve` starts\n  removed "+dir+" [coding]\n" {
		t.Errorf("down --name coding printed %q", out)
	}
	if got := labels(owners(t)); !slices.Equal(got, []string{dir}) {
		t.Errorf("after down --name coding: %q, want only the unnamed owner", got)
	}
	if out := cli(t, "down", dir, "--name", "coding"); !strings.HasSuffix(out, "  "+dir+" [coding] had no routes\n") {
		t.Errorf("second down --name coding printed %q", out)
	}

	upCoding(t, dir)
	if out := cli(t, "down", dir); !strings.HasSuffix(out, "  removed "+dir+"\n") {
		t.Errorf("down printed %q", out)
	}
	if got := labels(owners(t)); !slices.Equal(got, []string{dir + " [coding]"}) {
		t.Errorf("after down: %q, want only coding", got)
	}

	upMain(t, dir)
	if out := cli(t, "down", dir, "--all"); !strings.HasSuffix(out, "  removed "+dir+"\n  removed "+dir+" [coding]\n") {
		t.Errorf("down --all printed %q", out)
	}
	if got := owners(t); len(got) != 0 {
		t.Errorf("after down --all: %q, want none", labels(got))
	}
}

// TestUnnamedUpDownUseLegacyFile: a state file written before named owners (no name field, keyed
// by dir alone) is the unnamed owner that up replaces in place and down removes.
func TestUnnamedUpDownUseLegacyFile(t *testing.T) {
	dir := namedCheckout(t)
	ownersDir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "devingress", "owners")
	if err := os.MkdirAll(ownersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(dir))
	legacy := hex.EncodeToString(sum[:6]) + ".json"
	data := `{"dir": "` + dir + `", "file": "devingress.toml", "slug": "old", "sites": [{"host": "old.example.dev", "upstream": "127.0.0.1:1"}], "paths": null}`
	if err := os.WriteFile(filepath.Join(ownersDir, legacy), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := owners(t); len(got) != 1 || got[0].Name != "" || got[0].Slug != "old" {
		t.Fatalf("legacy owner loads as %+v", got)
	}

	upMain(t, dir)
	entries, _ := os.ReadDir(ownersDir)
	if len(entries) != 1 || entries[0].Name() != legacy {
		t.Fatalf("after up: %v, want only %s", entries, legacy)
	}
	if got := owners(t); got[0].Slug != "x" || got[0].Sites[0].Host != "main.example.dev" {
		t.Errorf("up did not replace the legacy owner: %+v", got)
	}

	cli(t, "down", dir)
	if entries, _ := os.ReadDir(ownersDir); len(entries) != 0 {
		t.Errorf("after down: %v, want none", entries)
	}
}

func TestNamedConflict(t *testing.T) {
	dir := namedCheckout(t)
	upMain(t, dir)
	code, _, stderr := runCLI(t, "up", dir, "--slug", "x", "--name", "coding")
	if want := "host main.example.dev is claimed by both " + dir + " and " + dir + " [coding]"; code != 1 || !strings.Contains(stderr, want) {
		t.Errorf("exit %d, stderr %q, want %q", code, stderr, want)
	}
	if got := labels(owners(t)); !slices.Equal(got, []string{dir}) {
		t.Errorf("owners after the rejected up: %q", got)
	}
}

func TestPruneEveryName(t *testing.T) {
	dir := namedCheckout(t)
	gone := filepath.Join(filepath.Dir(dir), "gone")
	registry(t,
		routefile.Owner{Dir: gone, Slug: "a", Sites: []routefile.Site{{Host: "a.example.dev", Upstream: "127.0.0.1:1"}}},
		routefile.Owner{Dir: gone, Name: "coding", Slug: "a", Sites: []routefile.Site{{Host: "b.example.dev", Upstream: "127.0.0.1:1"}}},
	)
	out := cli(t, "down", gone, "--name", "coding")
	want := "  pruned " + gone + " (directory is gone)\n  pruned " + gone + " [coding] (directory is gone)\n"
	if !strings.HasPrefix(out, want) || strings.Contains(out, "had no routes") {
		t.Errorf("down --name coding of a gone dir printed %q, want both pruned and no \"had no routes\"", out)
	}
	if got := owners(t); len(got) != 0 {
		t.Errorf("owners after prune: %q", labels(got))
	}
}

func TestNameUsageErrors(t *testing.T) {
	dir := namedCheckout(t)
	for _, args := range [][]string{
		{"up", dir, "--name", "a b"},
		{"up", dir, "--name", "[x]"},
		{"up", dir, "--name", "-x"},
		{"up", dir, "--name", ".x"},
		{"down", dir, "--name", "x/y"},
		{"down", dir, "--name", "x", "--all"},
	} {
		code, stdout, stderr := runCLI(t, args...)
		if code != 2 || stdout != "" || !strings.Contains(stderr, "usage: devingress "+args[0]) {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
	}
	if got := owners(t); len(got) != 0 {
		t.Errorf("owners after usage errors: %q", labels(got))
	}
}
