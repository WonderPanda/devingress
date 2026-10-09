package routefile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

const repoRoot = "../.."

// tomlBlocks returns the body of every ```toml fenced block in md, with the fence's indent removed.
func tomlBlocks(md string) []string {
	var blocks []string
	var cur []string
	indent, in := "", false
	for line := range strings.SplitSeq(md, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case !in && trimmed == "```toml":
			in, cur = true, nil
			indent = line[:len(line)-len(strings.TrimLeft(line, " "))]
		case in && trimmed == "```":
			in = false
			blocks = append(blocks, strings.Join(cur, "\n"))
		case in:
			cur = append(cur, strings.TrimPrefix(line, indent))
		}
	}
	return blocks
}

// TestSkillExamplesLoad loads every ```toml block of the setup skill as a devingress.toml, with
// each env file it names present and empty, so every ${VAR} must carry a default.
func TestSkillExamplesLoad(t *testing.T) {
	md, err := os.ReadFile(filepath.Join(repoRoot, "plugins/devingress/skills/setup/SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	blocks := tomlBlocks(string(md))
	if len(blocks) == 0 {
		t.Fatal("SKILL.md has no ```toml blocks")
	}
	for i, b := range blocks {
		var f file
		if _, err := toml.Decode(b, &f); err != nil {
			t.Errorf("block %d does not parse: %v\n%s", i+1, err, b)
			continue
		}
		files := map[string]string{DefaultName: b}
		for _, e := range f.Env {
			files[e] = ""
		}
		o, err := Load(write(t, files), DefaultName, "test")
		if err != nil {
			t.Errorf("block %d does not load: %v\n%s", i+1, err, b)
			continue
		}
		if len(o.Sites)+len(o.Paths) == 0 {
			t.Errorf("block %d registers no routes:\n%s", i+1, b)
		}
	}
}

var frontmatterName = regexp.MustCompile(`(?m)\A---\n(?:.*\n)*?name:\s*(\S+)\s*\n(?:.*\n)*?---\n`)

// TestPluginManifests checks the marketplace, plugin and skill manifests point at each other.
func TestPluginManifests(t *testing.T) {
	var market struct {
		Plugins []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"plugins"`
	}
	readJSON(t, filepath.Join(repoRoot, ".claude-plugin/marketplace.json"), &market)
	if len(market.Plugins) == 0 {
		t.Fatal("marketplace.json lists no plugins")
	}
	skills := map[string][]string{}
	for _, p := range market.Plugins {
		if !strings.HasPrefix(p.Source, "./") {
			t.Errorf("plugin %s: source %q does not start with ./", p.Name, p.Source)
			continue
		}
		dir := filepath.Join(repoRoot, p.Source)
		var plugin struct {
			Name string `json:"name"`
		}
		readJSON(t, filepath.Join(dir, ".claude-plugin/plugin.json"), &plugin)
		if plugin.Name != p.Name {
			t.Errorf("plugin.json in %s: name %q, marketplace says %q", p.Source, plugin.Name, p.Name)
		}
		entries, err := os.ReadDir(filepath.Join(dir, "skills"))
		if err != nil {
			t.Errorf("plugin %s: %v", p.Name, err)
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			md, err := os.ReadFile(filepath.Join(dir, "skills", e.Name(), "SKILL.md"))
			if err != nil {
				t.Errorf("plugin %s: %v", p.Name, err)
				continue
			}
			m := frontmatterName.FindSubmatch(md)
			if m == nil || string(m[1]) != e.Name() {
				t.Errorf("skills/%s/SKILL.md: frontmatter name must be %q", e.Name(), e.Name())
			}
			skills[p.Name] = append(skills[p.Name], e.Name())
		}
	}
	if got := skills["devingress"]; !slices.Contains(got, "setup") {
		t.Errorf("devingress plugin skills = %v, want setup among them", got)
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
