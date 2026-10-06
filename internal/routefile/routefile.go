// Package routefile reads a project's porch.toml and resolves it, against its
// env files and slug, into the concrete routes one owner contributes.
package routefile

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

const DefaultName = "porch.toml"

type file struct {
	Domain string      `toml:"domain"`
	Env    []string    `toml:"env"`
	Routes []fileRoute `toml:"route"`
	Paths  []filePath  `toml:"path"`
}

type fileRoute struct {
	Host      string   `toml:"host"`
	To        string   `toml:"to"`
	When      string   `toml:"when"`
	Retry     string   `toml:"retry"`
	Protocols []string `toml:"protocols"`
	Proxy     string   `toml:"proxy"`
	Caddy     string   `toml:"caddy"`
}

type filePath struct {
	Prefix string `toml:"prefix"`
	To     string `toml:"to"`
	When   string `toml:"when"`
	Strip  bool   `toml:"strip"`
	Proxy  string `toml:"proxy"`
}

// Site is one HTTPS virtual host.
type Site struct {
	Host      string   `json:"host"`
	Upstream  string   `json:"upstream"`
	Retry     string   `json:"retry,omitempty"`
	Protocols []string `json:"protocols,omitempty"`
	Proxy     string   `json:"proxy,omitempty"` // raw directives inside reverse_proxy
	Caddy     string   `json:"caddy,omitempty"` // raw site body; replaces the generated one
}

// PathRoute is a prefix on the plain-HTTP listener, for containers that call
// the host (host.docker.internal) rather than a hostname.
type PathRoute struct {
	Prefix   string `json:"prefix"`
	Upstream string `json:"upstream"`
	Strip    bool   `json:"strip,omitempty"`
	Proxy    string `json:"proxy,omitempty"`
}

// Owner is everything one directory contributes. Its routes live exactly as
// long as Dir exists.
type Owner struct {
	Dir   string      `json:"dir"`
	File  string      `json:"file"`
	Slug  string      `json:"slug"`
	Sites []Site      `json:"sites"`
	Paths []PathRoute `json:"paths"`
}

// Load reads <dir>/<name> and resolves it. slug == "" derives it from the
// directory's git branch.
func Load(dir, name, slug string) (Owner, error) {
	path := filepath.Join(dir, name)
	var f file
	if _, err := toml.DecodeFile(path, &f); err != nil {
		return Owner{}, fmt.Errorf("%s: %w", path, err)
	}
	if slug == "" {
		var err error
		if slug, err = gitSlug(dir); err != nil {
			return Owner{}, fmt.Errorf("%s: no --slug given and %w", path, err)
		}
	}
	vars := map[string]string{}
	for _, e := range f.Env {
		if err := readEnvFile(filepath.Join(dir, e), vars); err != nil {
			return Owner{}, fmt.Errorf("%s: env %q: %w", path, e, err)
		}
	}
	r := resolver{slug: slug, vars: vars}
	o := Owner{Dir: dir, File: name, Slug: slug}

	for i, fr := range f.Routes {
		at := fmt.Sprintf("%s: route[%d]", path, i)
		if !r.enabled(fr.When) {
			continue
		}
		if fr.Caddy != "" && (fr.Proxy != "" || fr.Retry != "") {
			return Owner{}, fmt.Errorf("%s: caddy replaces the generated site body, so it cannot be combined with proxy or retry", at)
		}
		host, err := r.expand(fr.Host, nil)
		if err != nil || host == "" {
			return Owner{}, fmt.Errorf("%s: host: %v", at, orEmpty(err, "required"))
		}
		if f.Domain != "" {
			host += "." + f.Domain
		}
		up, err := r.upstream(fr.To)
		if err != nil {
			return Owner{}, fmt.Errorf("%s (%s): to: %w", at, host, err)
		}
		local := map[string]string{"upstream": up}
		proxy, err := r.expand(fr.Proxy, local)
		if err != nil {
			return Owner{}, fmt.Errorf("%s (%s): proxy: %w", at, host, err)
		}
		caddy, err := r.expand(fr.Caddy, local)
		if err != nil {
			return Owner{}, fmt.Errorf("%s (%s): caddy: %w", at, host, err)
		}
		o.Sites = append(o.Sites, Site{Host: host, Upstream: up, Retry: fr.Retry, Protocols: fr.Protocols, Proxy: proxy, Caddy: caddy})
	}

	for i, fp := range f.Paths {
		at := fmt.Sprintf("%s: path[%d]", path, i)
		if !r.enabled(fp.When) {
			continue
		}
		prefix, err := r.expand(fp.Prefix, nil)
		if err != nil || !strings.HasPrefix(prefix, "/") {
			return Owner{}, fmt.Errorf("%s: prefix must start with /: %v", at, orEmpty(err, prefix))
		}
		up, err := r.upstream(fp.To)
		if err != nil {
			return Owner{}, fmt.Errorf("%s (%s): to: %w", at, prefix, err)
		}
		proxy, err := r.expand(fp.Proxy, map[string]string{"upstream": up})
		if err != nil {
			return Owner{}, fmt.Errorf("%s (%s): proxy: %w", at, prefix, err)
		}
		o.Paths = append(o.Paths, PathRoute{Prefix: strings.TrimSuffix(prefix, "/"), Upstream: up, Strip: fp.Strip, Proxy: proxy})
	}
	return o, nil
}

func orEmpty(err error, fallback string) string {
	if err != nil {
		return err.Error()
	}
	return fallback
}

// ${NAME}: distinct from Caddy's own {$ENV} and {placeholder} syntax, so raw
// Caddy text passes through untouched apart from porch's variables.
var varRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

type resolver struct {
	slug string
	vars map[string]string
}

// lookup: built-ins, then the declared env files, then the process env.
func (r resolver) lookup(name string, local map[string]string) (string, bool) {
	if v, ok := local[name]; ok {
		return v, true
	}
	if name == "slug" {
		return r.slug, true
	}
	if v, ok := r.vars[name]; ok {
		return v, true
	}
	return os.LookupEnv(name)
}

func (r resolver) expand(s string, local map[string]string) (string, error) {
	var missing []string
	out := varRe.ReplaceAllStringFunc(s, func(m string) string {
		name := varRe.FindStringSubmatch(m)[1]
		v, ok := r.lookup(name, local)
		if !ok {
			missing = append(missing, name)
		}
		return v
	})
	if missing != nil {
		return "", fmt.Errorf("undefined variable(s): %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// enabled: an unset variable in `when` means off, not an error.
func (r resolver) enabled(when string) bool {
	if when == "" {
		return true
	}
	v := varRe.ReplaceAllStringFunc(when, func(m string) string {
		s, _ := r.lookup(varRe.FindStringSubmatch(m)[1], nil)
		return s
	})
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

var portRe = regexp.MustCompile(`^[0-9]+$`)

// upstream: a bare port means this host; anything else is passed to Caddy as-is.
func (r resolver) upstream(to string) (string, error) {
	v, err := r.expand(to, nil)
	if err != nil {
		return "", err
	}
	v = strings.TrimSpace(v)
	switch {
	case v == "":
		return "", fmt.Errorf("required")
	case portRe.MatchString(v):
		return "127.0.0.1:" + v, nil
	}
	return v, nil
}

func readEnvFile(path string, into map[string]string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		into[strings.TrimSpace(k)] = v
	}
	return sc.Err()
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func gitSlug(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("%s is not a git checkout", dir)
	}
	return Slugify(strings.TrimSpace(string(out))), nil
}

// Slugify lowercases and collapses everything outside [a-z0-9] to single dashes.
func Slugify(s string) string {
	return strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
}
