package state

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/WonderPanda/devingress/internal/routefile"
)

func TestUnnamedKeyUnchanged(t *testing.T) {
	// hex(sha256(dir)[:6]), the key every state file written before named owners has.
	if got := key("/home/u/code/eluve-nx", ""); got != "f9289a016faf" {
		t.Errorf("unnamed key = %s, want f9289a016faf", got)
	}
	named := key("/home/u/code/eluve-nx", "coding")
	if named == "f9289a016faf" || named == key("/home/u/code/eluve-nx", "sandbox") {
		t.Errorf("named key %s collides", named)
	}
}

func files(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestLegacyStateFile(t *testing.T) {
	s := New(t.TempDir())
	if err := os.MkdirAll(s.ownersDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	// A state file as written before named owners: no "name" field.
	legacy := `{"dir": "/home/u/code/eluve-nx", "file": "devingress.toml", "slug": "main",
  "sites": [{"host": "web-main.eluve.dev", "upstream": "127.0.0.1:3000"}], "paths": null}`
	if err := os.WriteFile(filepath.Join(s.ownersDir(), "f9289a016faf.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	owners, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(owners) != 1 || owners[0].Dir != "/home/u/code/eluve-nx" || owners[0].Name != "" || len(owners[0].Sites) != 1 {
		t.Fatalf("List = %+v, want the legacy owner, unnamed", owners)
	}

	if err := s.Save(routefile.Owner{Dir: "/home/u/code/eluve-nx", Name: "coding", Slug: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(routefile.Owner{Dir: "/home/u/code/eluve-nx", Slug: "other"}); err != nil {
		t.Fatal(err)
	}
	named := key("/home/u/code/eluve-nx", "coding") + ".json"
	if got, want := files(t, s.ownersDir()), slices.Sorted(slices.Values([]string{"f9289a016faf.json", named})); !slices.Equal(got, want) {
		t.Fatalf("owner files = %v, want %v", got, want)
	}
	owners, err = s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(owners) != 2 || owners[0].Name != "" || owners[0].Slug != "other" || owners[1].Name != "coding" {
		t.Fatalf("List = %+v, want the replaced unnamed owner, then coding", owners)
	}

	if err := s.Remove("/home/u/code/eluve-nx", ""); err != nil {
		t.Fatal(err)
	}
	if got := files(t, s.ownersDir()); !slices.Equal(got, []string{named}) {
		t.Fatalf("after removing the unnamed owner: %v, want only %s", got, named)
	}
}
