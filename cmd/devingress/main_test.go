package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/WonderPanda/devingress/internal/routefile"
	"github.com/WonderPanda/devingress/internal/state"
)

// isolate points every XDG dir at a temp root, so nothing here can reach a
// real devingress's state, config or admin socket.
func isolate(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, v := range []string{"XDG_STATE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR"} {
		t.Setenv(v, filepath.Join(root, strings.ToLower(v)))
	}
	return root
}

func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestHelpIsTheSameEveryWay(t *testing.T) {
	isolate(t)
	want := []string{"caddyfile", "dns", "doctor", "down", "routes", "serve", "service", "status", "trust", "up", "version"}
	if got := slices.Sorted(func(yield func(string) bool) {
		for name := range commands {
			if !yield(name) {
				return
			}
		}
	}); !slices.Equal(got, want) {
		t.Errorf("commands = %v, want %v", got, want)
	}
	for name, cmd := range commands {
		if !strings.Contains(cmd.help, "Exit codes:") || !strings.Contains(cmd.help, "devingress "+name) {
			t.Errorf("%s: help needs exit codes and an example", name)
		}
		var first string
		for _, args := range [][]string{{"help", name}, {name, "--help"}, {name, "-h"}} {
			code, stdout, stderr := runCLI(t, args...)
			if code != 0 || stderr != "" {
				t.Errorf("%v: exit %d, stderr %q", args, code, stderr)
			}
			if !strings.HasPrefix(stdout, "usage: devingress "+name) {
				t.Errorf("%v: stdout starts %q", args, stdout[:min(len(stdout), 40)])
			}
			if first == "" {
				first = stdout
			} else if stdout != first {
				t.Errorf("%v: output differs from help %s", args, name)
			}
		}
	}
}

func TestTopLevelHelp(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"-h"}, {"--help"}} {
		code, stdout, stderr := runCLI(t, args...)
		if code != 0 || stderr != "" || stdout != usage {
			t.Errorf("%v: exit %d, stderr %q, stdout is not the usage", args, code, stderr)
		}
	}
	code, stdout, _ := runCLI(t, "help", "routefile")
	if code != 0 || stdout != routefile.Help {
		t.Errorf("help routefile: exit %d", code)
	}
	for name := range commands {
		if !strings.Contains(usage, "devingress "+commands[name].synopsis+" ") {
			t.Errorf("usage does not list %q", commands[name].synopsis)
		}
	}
}

func TestHelpWidth(t *testing.T) {
	texts := map[string]string{"usage": usage, "routefile": routefile.Help}
	for name, cmd := range commands {
		texts[name] = cmd.text()
	}
	for name, text := range texts {
		for i, line := range strings.Split(text, "\n") {
			if len(line) > 100 {
				t.Errorf("%s line %d is %d columns: %q", name, i+1, len(line), line)
			}
		}
	}
}

func TestUsageErrors(t *testing.T) {
	isolate(t)
	code, stdout, stderr := runCLI(t)
	if code != 2 || stdout != "" || stderr != usage {
		t.Errorf("no args: exit %d, stdout %q", code, stdout)
	}
	code, stdout, stderr = runCLI(t, "bogus")
	if code != 2 || stdout != "" || !strings.Contains(stderr, `unknown command "bogus"`) || !strings.Contains(stderr, usage) {
		t.Errorf("unknown command: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	code, _, stderr = runCLI(t, "help", "bogus")
	if code != 2 || !strings.Contains(stderr, usage) {
		t.Errorf("help bogus: exit %d, stderr %q", code, stderr)
	}
	for name, cmd := range commands {
		bad := [][]string{{name, "--no-such-flag"}}
		switch name {
		case "up", "down":
			bad = append(bad, []string{name, "a", "b"})
		default:
			bad = append(bad, []string{name, "extra"})
		}
		for _, args := range bad {
			code, stdout, stderr := runCLI(t, args...)
			if code != 2 || stdout != "" || !strings.Contains(stderr, "usage: devingress "+cmd.synopsis+"\n") {
				t.Errorf("%v: exit %d, stdout %q, stderr %q", args, code, stdout, stderr)
			}
		}
	}
}

// registry saves owners straight into the isolated state dir: routes reads
// only that, so there is no need to go through up.
func registry(t *testing.T, owners ...routefile.Owner) {
	t.Helper()
	store := state.New(filepath.Join(os.Getenv("XDG_STATE_HOME"), "devingress"))
	for _, o := range owners {
		if err := store.Save(o); err != nil {
			t.Fatal(err)
		}
	}
}

// checkouts registers three owners: a and b exist, gone does not.
func checkouts(t *testing.T) (root, a, b, gone string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(isolate(t))
	if err != nil {
		t.Fatal(err)
	}
	a, b, gone = filepath.Join(root, "a"), filepath.Join(root, "b"), filepath.Join(root, "gone")
	for _, d := range []string{a, b} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	registry(t,
		routefile.Owner{Dir: a, File: "devingress.toml", Slug: "main", Sites: []routefile.Site{
			{Host: "web-main.example.dev", Upstream: "127.0.0.1:3000", Retry: "5s"},
			{Host: "api-main.example.dev", Upstream: "127.0.0.1:3001", Protocols: []string{"h1"}, Caddy: "reverse_proxy x"},
		}, Paths: []routefile.PathRoute{{Prefix: "/auth-main", Upstream: "127.0.0.1:3002", Strip: true, Proxy: "header_down X 1"}}},
		routefile.Owner{Dir: b, File: "devingress.toml", Slug: "feat", Sites: []routefile.Site{
			{Host: "web-feat.example.dev", Upstream: "127.0.0.1:4000"},
		}},
		routefile.Owner{Dir: gone, File: "other.toml", Slug: "old", Sites: []routefile.Site{
			{Host: "web-old.example.dev", Upstream: "127.0.0.1:5000"},
		}},
	)
	return root, a, b, gone
}

// pinHome points HOME outside every registered owner, so text output prints
// absolute dirs wherever t.TempDir sits.
func pinHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func routesText(t *testing.T, args ...string) []string {
	t.Helper()
	code, stdout, stderr := runCLI(t, append([]string{"routes"}, args...)...)
	if code != 0 || stderr != "" {
		t.Fatalf("%v: exit %d, stderr %q", args, code, stderr)
	}
	if !strings.HasSuffix(stdout, "\n") {
		t.Fatalf("%v: stdout %q does not end in a newline", args, stdout)
	}
	return strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
}

func TestRoutesAllOwners(t *testing.T) {
	_, a, b, gone := checkouts(t)
	pinHome(t)
	want := []string{
		a,
		"  https://api-main.example.dev  127.0.0.1:3001",
		"  https://web-main.example.dev  127.0.0.1:3000",
		"  http://<host>/auth-main/*     127.0.0.1:3002",
		"",
		b,
		"  https://web-feat.example.dev  127.0.0.1:4000",
		"",
		gone + "  [gone]",
		"  https://web-old.example.dev  127.0.0.1:5000",
	}
	if got := routesText(t); !slices.Equal(got, want) {
		t.Errorf("routes:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestRoutesHomeShortened(t *testing.T) {
	root, a, b, _ := checkouts(t)
	sub, sibling := filepath.Join(a, "sub"), a+"2"
	registry(t,
		routefile.Owner{Dir: sub, Slug: "sub", Sites: []routefile.Site{{Host: "web-sub.example.dev", Upstream: "127.0.0.1:6000"}}},
		routefile.Owner{Dir: sibling, Slug: "a2", Sites: []routefile.Site{{Host: "web-a2.example.dev", Upstream: "127.0.0.1:7000"}}},
	)
	t.Setenv("HOME", a)
	var headers []string
	for _, line := range routesText(t) {
		if line != "" && !strings.HasPrefix(line, " ") {
			headers = append(headers, line)
		}
	}
	want := []string{"~", "~/sub  [gone]", sibling + "  [gone]", b, filepath.Join(root, "gone") + "  [gone]"}
	if !slices.Equal(headers, want) {
		t.Errorf("headers with HOME=%s:\n%s\nwant:\n%s", a, strings.Join(headers, "\n"), strings.Join(want, "\n"))
	}
	if routes := routesJSON(t, 0, "--dir", a); routes[0].Dir != a {
		t.Errorf("--json dir = %s, want the absolute %s", routes[0].Dir, a)
	}
}

func TestTildeHome(t *testing.T) {
	for _, c := range []struct{ dir, home, want string }{
		{"/home/u", "/home/u", "~"},
		{"/home/u/x/y", "/home/u", "~/x/y"},
		{"/home/u/x", "/home/u/", "~/x"},
		{"/home/u2/x", "/home/u", "/home/u2/x"},
		{"/srv/x", "/home/u", "/srv/x"},
		{"/home/u/x", "", "/home/u/x"},
	} {
		if got := tildeHome(c.dir, c.home); got != c.want {
			t.Errorf("tildeHome(%q, %q) = %q, want %q", c.dir, c.home, got, c.want)
		}
	}
}

type jsonRoute struct {
	Kind      string    `json:"kind"`
	URL       *string   `json:"url"`
	Host      *string   `json:"host"`
	Prefix    *string   `json:"prefix"`
	Upstream  string    `json:"upstream"`
	Retry     *string   `json:"retry"`
	Protocols *[]string `json:"protocols"`
	Strip     *bool     `json:"strip"`
	Dir       string    `json:"dir"`
	Name      *string   `json:"name"`
	File      string    `json:"file"`
	Slug      string    `json:"slug"`
	Gone      *bool     `json:"gone"`
	Caddy     *string   `json:"caddy"`
	Proxy     *string   `json:"proxy"`
}

func routesJSON(t *testing.T, wantCode int, args ...string) []jsonRoute {
	t.Helper()
	code, stdout, stderr := runCLI(t, append([]string{"routes", "--json"}, args...)...)
	if code != wantCode {
		t.Fatalf("%v: exit %d, want %d; stderr %q", args, code, wantCode, stderr)
	}
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.DisallowUnknownFields()
	var routes []jsonRoute
	if err := dec.Decode(&routes); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, stdout)
	}
	if routes == nil {
		t.Fatalf("%v: printed %q, want an array", args, stdout)
	}
	return routes
}

func TestRoutesJSON(t *testing.T) {
	_, a, _, gone := checkouts(t)
	routes := routesJSON(t, 0)
	if len(routes) != 5 {
		t.Fatalf("got %d routes, want 5", len(routes))
	}
	for _, r := range routes {
		if r.Caddy != nil || r.Proxy != nil {
			t.Errorf("%+v: raw caddy/proxy snippets must be left out", r)
		}
	}
	site := routes[0]
	if site.Kind != "site" || *site.URL != "https://api-main.example.dev" || *site.Host != "api-main.example.dev" ||
		site.Upstream != "127.0.0.1:3001" || *site.Retry != "" || !slices.Equal(*site.Protocols, []string{"h1"}) ||
		site.Dir != a || site.File != "devingress.toml" || site.Slug != "main" || *site.Gone || site.Prefix != nil || site.Strip != nil {
		t.Errorf("site: %+v", site)
	}
	if feat := routes[1]; *feat.Protocols == nil || len(*feat.Protocols) != 0 {
		t.Errorf("protocols of a site without any = %v, want []", feat.Protocols)
	}
	if old := routes[3]; old.Dir != gone || !*old.Gone || old.File != "other.toml" {
		t.Errorf("gone site: %+v", old)
	}
	path := routes[4]
	if path.Kind != "path" || *path.Prefix != "/auth-main" || path.Upstream != "127.0.0.1:3002" || !*path.Strip ||
		path.Dir != a || path.Slug != "main" || *path.Gone || path.URL != nil || path.Host != nil || path.Protocols != nil {
		t.Errorf("path: %+v", path)
	}
}

func TestRoutesDir(t *testing.T) {
	root, a, _, gone := checkouts(t)
	if err := os.Symlink(a, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	for _, d := range []string{a, "a", "./link", filepath.Join(root, "link"), "b/../a"} {
		routes := routesJSON(t, 0, "--dir", d)
		if len(routes) != 3 {
			t.Errorf("--dir %s: %d routes, want a's 3", d, len(routes))
		}
		for _, r := range routes {
			if r.Dir != a {
				t.Errorf("--dir %s: route of %s", d, r.Dir)
			}
		}
	}
	t.Chdir(a)
	if routes := routesJSON(t, 0, "--dir", "."); len(routes) != 3 {
		t.Errorf("--dir . from a: %d routes, want 3", len(routes))
	}
	if routes := routesJSON(t, 0, "--dir", gone); len(routes) != 1 || !*routes[0].Gone {
		t.Errorf("--dir of a gone owner: %+v", routes)
	}
}

func TestRoutesHost(t *testing.T) {
	_, _, b, _ := checkouts(t)
	routes := routesJSON(t, 0, "--host", "web-feat.example.dev")
	if len(routes) != 1 || *routes[0].Host != "web-feat.example.dev" || routes[0].Dir != b {
		t.Errorf("--host: %+v", routes)
	}
	pinHome(t)
	if got, want := routesText(t, "--host", "web-feat.example.dev"), []string{b, "  https://web-feat.example.dev  127.0.0.1:4000"}; !slices.Equal(got, want) {
		t.Errorf("--host text: %q, want %q", got, want)
	}
	if routes := routesJSON(t, 0, "--host", "web-feat.example.dev", "--dir", b); len(routes) != 1 {
		t.Errorf("--host with its owner's --dir: %+v", routes)
	}
}

func TestRoutesMiss(t *testing.T) {
	root, a, _, _ := checkouts(t)
	for _, args := range [][]string{
		{"--host", "nope.example.dev"},
		{"--host", "web-main"},
		{"--dir", filepath.Join(root, "unregistered")},
		{"--dir", a, "--host", "web-feat.example.dev"},
	} {
		if routes := routesJSON(t, 1, args...); len(routes) != 0 {
			t.Errorf("%v: %+v, want []", args, routes)
		}
		code, stdout, stderr := runCLI(t, append([]string{"routes"}, args...)...)
		if code != 1 || stdout != "" || strings.Count(stderr, "\n") != 1 || !strings.HasPrefix(stderr, "devingress: no ") {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
	}
}

func TestRoutesHTTPSPort(t *testing.T) {
	root, _, _, _ := checkouts(t)
	confDir := filepath.Join(root, "xdg_config_home", "devingress")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "config.toml"), []byte("https_port = 8443\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	routes := routesJSON(t, 0, "--host", "web-feat.example.dev")
	if len(routes) != 1 || *routes[0].URL != "https://web-feat.example.dev:8443" {
		t.Errorf("--json with https_port 8443: %+v", routes)
	}
	if lines := routesText(t, "--host", "web-feat.example.dev"); len(lines) != 2 || strings.Fields(lines[1])[0] != "https://web-feat.example.dev:8443" {
		t.Errorf("text with https_port 8443: %q", lines)
	}
}

func TestRoutesEmptyRegistry(t *testing.T) {
	isolate(t)
	code, stdout, stderr := runCLI(t, "routes", "--json")
	if code != 0 || stdout != "[]\n" || stderr != "" {
		t.Errorf("--json: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	code, stdout, stderr = runCLI(t, "routes")
	if code != 0 || stdout != "" || stderr != "" {
		t.Errorf("text: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestStatusProbesUpstreams(t *testing.T) {
	root, err := filepath.EvalSymlinks(isolate(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("NO_COLOR", "")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := closed.Addr().String()
	closed.Close()

	registry(t, routefile.Owner{Dir: root, File: "devingress.toml", Slug: "main",
		Sites: []routefile.Site{
			{Host: "live.example.dev", Upstream: ln.Addr().String()},
			{Host: "dead.example.dev", Upstream: dead},
			{Host: "dyn.example.dev", Upstream: "{http.request.header.X-To}"},
		},
		Paths: []routefile.PathRoute{{Prefix: "/auth", Upstream: dead}}})

	code, stdout, stderr := runCLI(t, "status")
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	for _, want := range []string{
		"  up    https://live.example.dev -> " + ln.Addr().String() + "\n",
		"  down  https://dead.example.dev -> " + dead + "\n",
		"  ?     https://dyn.example.dev -> {http.request.header.X-To}\n",
		"  down  http://<host>/auth/* -> " + dead + "\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in:\n%s", want, stdout)
		}
	}
}
