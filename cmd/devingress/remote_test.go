package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeToken matches caddy-dns/cloudflare's format check; it is no real token.
const fakeToken = "cfut_" + "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN"

// remoteMachine isolates the CLI, gives it a config.toml with [remote] and a token file, and
// returns a checkout whose route file opts in with remote = true.
func remoteMachine(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(isolate(t))
	if err != nil {
		t.Fatal(err)
	}
	pinHome(t)
	t.Setenv("CAROOT", t.TempDir()) // no mkcert root: Caddy mints one in the temp data dir
	cfgDir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "devingress")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "cloudflare-token"), []byte(fakeToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := "https_port = 18443\n[remote]\nsuffix = \"machine.example.dev\"\ndns_provider = \"cloudflare\"\ntoken_file = \"cloudflare-token\"\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "app")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	routes := `domain = "example.dev"
remote = true

[[route]]
host = "web-${slug}"
to = "3000"

[[route]]
host = "docs.${slug}"
to = "3001"

[[path]]
prefix = "/auth"
to = "3002"
`
	if err := os.WriteFile(filepath.Join(dir, "devingress.toml"), []byte(routes), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRemoteURLs(t *testing.T) {
	dir := remoteMachine(t)
	var all []string // every output, checked for the token at the end

	up := cli(t, "up", dir, "--slug", "my-branch")
	all = append(all, up)
	for _, want := range []string{
		"  https://web-my-branch.example.dev -> localhost:3000  (remote https://web-my-branch.machine.example.dev)\n",
		"  https://docs.my-branch.example.dev -> localhost:3001\n",
		`  warning: docs.my-branch.example.dev gets no remote URL: "docs.my-branch" is 2 DNS labels, and the wildcard certificate *.machine.example.dev covers one` + "\n",
		"  http://<host>/auth/* -> localhost:3002\n",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("up missing %q:\n%s", want, up)
		}
	}

	routes := routesJSON(t, 0, "--dir", dir)
	var remoteURLs []string
	for _, r := range routes {
		if r.RemoteURL != nil {
			remoteURLs = append(remoteURLs, *r.RemoteURL)
		}
	}
	if want := []string{"", "https://web-my-branch.machine.example.dev"}; !slices.Equal(remoteURLs, want) {
		t.Errorf("remote_url of the sites = %q, want %q", remoteURLs, want)
	}
	lines := routesText(t)
	all = append(all, strings.Join(lines, "\n"))
	want := []string{"https://web-my-branch.example.dev:18443", "localhost:3000", "https://web-my-branch.machine.example.dev"}
	if !slices.ContainsFunc(lines, func(l string) bool { return slices.Equal(strings.Fields(l), want) }) {
		t.Errorf("routes has no line %q:\n%s", want, strings.Join(lines, "\n"))
	}

	status := cli(t, "status")
	all = append(all, status)
	if !strings.Contains(status, "https://web-my-branch.example.dev -> localhost:3000  (remote https://web-my-branch.machine.example.dev)\n") {
		t.Errorf("status:\n%s", status)
	}

	caddy := cli(t, "caddyfile")
	all = append(all, caddy)
	for _, want := range []string{"web-my-branch.machine.example.dev {\n", "*.machine.example.dev {\n", "strict_sni_host on\n"} {
		if !strings.Contains(caddy, want) {
			t.Errorf("caddyfile missing %q", want)
		}
	}
	for _, out := range all {
		if strings.Contains(out, fakeToken) {
			t.Fatalf("output quotes the token:\n%s", out)
		}
	}
}

// A token file that no longer holds a token makes up fail naming the file, before Caddy (whose
// error would quote the content) ever reads it.
func TestRemoteBadTokenFile(t *testing.T) {
	dir := remoteMachine(t)
	tokenFile := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "devingress", "cloudflare-token")
	bad := "Bearer " + fakeToken
	if err := os.WriteFile(tokenFile, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "up", dir, "--slug", "x")
	if code != 1 || !strings.Contains(stderr, "token file "+tokenFile+" does not hold a Cloudflare API token") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
	if strings.Contains(stdout+stderr, fakeToken) {
		t.Errorf("output quotes the token: %q %q", stdout, stderr)
	}
}
