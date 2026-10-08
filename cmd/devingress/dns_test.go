package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/WonderPanda/devingress/internal/service"
)

// fakeDNS replaces everything dns touches: resolved runs, the drop-in dir holds confs, resolved's
// global servers are *global, and the runner records each step instead of running it.
func fakeDNS(t *testing.T, confs map[string]string) (global *[]string, ran *[]service.Step) {
	t.Helper()
	isolate(t)
	global, ran = &[]string{}, &[]service.Step{}
	supported, readConfs, servers, run := dnsSupported, dnsConfs, dnsGlobalServers, dnsRun
	t.Cleanup(func() { dnsSupported, dnsConfs, dnsGlobalServers, dnsRun = supported, readConfs, servers, run })
	dnsSupported = func() error { return nil }
	dnsConfs = func() (map[string]string, error) { return confs, nil }
	dnsGlobalServers = func() ([]string, error) { return slices.Clone(*global), nil }
	dnsRun = func(s service.Step, _ io.Writer) error {
		*ran = append(*ran, s)
		return nil
	}
	return global, ran
}

const (
	dropInPath    = "/etc/systemd/resolved.conf.d/devingress-example.dev.conf"
	plainDropIn   = "[Resolve]\nDNS=127.0.0.1:15353\nDomains=~example.dev\n"
	restartCmd    = "sudo systemctl restart systemd-resolved"
	resolvedDirIn = "/etc/systemd/resolved.conf.d"
)

var setupCommands = []string{"sudo mkdir -p " + resolvedDirIn, "sudo tee " + dropInPath, restartCmd}

func TestDNSSetup(t *testing.T) {
	_, ran := fakeDNS(t, map[string]string{"10-disable-multicast.conf": "[Resolve]\nLLMNR=no\n"})
	code, stdout, stderr := runCLI(t, "dns", "setup", "Example.dev")
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if got := stepLines(*ran); !slices.Equal(got, setupCommands) {
		t.Errorf("ran %q, want %q", got, setupCommands)
	}
	if (*ran)[1].Stdin != plainDropIn {
		t.Errorf("tee got:\n%s\nwant:\n%s", (*ran)[1].Stdin, plainDropIn)
	}
	if want := "+ " + strings.Join(setupCommands, "\n+ ") + "\n"; stdout != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", stdout, want)
	}
}

func TestDNSSetupPrint(t *testing.T) {
	_, ran := fakeDNS(t, nil)
	want := "sudo mkdir -p " + resolvedDirIn + "\n" +
		"sudo tee " + dropInPath + " >/dev/null <<'EOF'\n" + plainDropIn + "EOF\n" +
		restartCmd + "\n"
	for _, args := range [][]string{{"dns", "setup", "example.dev", "--print"}, {"dns", "--print", "setup", "example.dev"}} {
		code, stdout, stderr := runCLI(t, args...)
		if code != 0 || stderr != "" || stdout != want {
			t.Errorf("%v: exit %d, stderr %q, stdout:\n%s\nwant:\n%s", args, code, stderr, stdout, want)
		}
	}
	if len(*ran) != 0 {
		t.Errorf("--print ran %q", stepLines(*ran))
	}
}

func TestDNSSetupDNSListen(t *testing.T) {
	_, ran := fakeDNS(t, nil)
	conf := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "devingress", "config.toml")
	if err := os.MkdirAll(filepath.Dir(conf), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conf, []byte(`dns_listen = "127.0.0.1:5300"`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCLI(t, "dns", "setup", "example.dev"); code != 0 ||
		(*ran)[1].Stdin != "[Resolve]\nDNS=127.0.0.1:5300\nDomains=~example.dev\n" {
		t.Errorf("exit %d, stderr %q, ran %+v", code, stderr, *ran)
	}

	*ran = nil
	if err := os.WriteFile(conf, []byte(`dns_listen = ""`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCLI(t, "dns", "setup", "example.dev", "--print"); code != 1 ||
		!strings.Contains(stderr, "dns_listen is empty") || len(*ran) != 0 {
		t.Errorf("dns_listen off: exit %d, stderr %q, ran %q", code, stderr, stepLines(*ran))
	}
}

func TestDNSSetupGlobalServers(t *testing.T) {
	global, ran := fakeDNS(t, nil)
	*global = []string{"1.1.1.1#cloudflare-dns.com", "127.0.0.1:15353", "9.9.9.9"}

	for _, args := range [][]string{{"dns", "setup", "example.dev"}, {"dns", "setup", "example.dev", "--print"}} {
		code, stdout, stderr := runCLI(t, args...)
		if code != 1 || stdout != "" || len(*ran) != 0 ||
			!strings.Contains(stderr, "global DNS servers: 1.1.1.1#cloudflare-dns.com 9.9.9.9\n") ||
			!strings.Contains(stderr, "--reset-global-dns") {
			t.Errorf("%v: exit %d, stdout %q, ran %q, stderr:\n%s", args, code, stdout, stepLines(*ran), stderr)
		}
	}

	reset := `[Resolve]
# Cleared: global DNS servers are asked before devingress for ~example.dev and
# answer NXDOMAIN for a real domain. Other lookups use per-link DNS.
DNS=
DNS=127.0.0.1:15353
Domains=~example.dev
`
	code, stdout, stderr := runCLI(t, "dns", "setup", "example.dev", "--reset-global-dns")
	if code != 0 || stderr != "" || !slices.Equal(stepLines(*ran), setupCommands) || (*ran)[1].Stdin != reset {
		t.Fatalf("--reset-global-dns: exit %d, stderr %q, ran %+v", code, stderr, *ran)
	}
	warning, rest, _ := strings.Cut(stdout, "+ ")
	if !strings.HasPrefix(warning, "Warning: --reset-global-dns removes 1.1.1.1#cloudflare-dns.com 9.9.9.9 from") ||
		!strings.Contains(warning, "every lookup on this machine, not just example.dev") ||
		!strings.HasPrefix(rest, setupCommands[0]) {
		t.Errorf("--reset-global-dns stdout:\n%s", stdout)
	}

	*ran = nil
	code, stdout, _ = runCLI(t, "dns", "setup", "example.dev", "--reset-global-dns", "--print")
	if code != 0 || len(*ran) != 0 || !strings.HasPrefix(stdout, "# Warning: --reset-global-dns removes") ||
		!strings.Contains(stdout, "\nDNS=\nDNS=127.0.0.1:15353\n") {
		t.Errorf("--reset-global-dns --print: exit %d, ran %q, stdout:\n%s", code, stepLines(*ran), stdout)
	}

	// Only devingress itself, from another domain's drop-in: nothing shadows it.
	*global, *ran = []string{"127.0.0.1:15353"}, nil
	if code, _, _ := runCLI(t, "dns", "setup", "example.dev"); code != 0 || (*ran)[1].Stdin != plainDropIn {
		t.Errorf("only devingress global: exit %d, ran %+v", code, *ran)
	}
}

func TestDNSSetupRoutedElsewhere(t *testing.T) {
	_, ran := fakeDNS(t, map[string]string{
		"eluve-dev.conf": "[Resolve]\nDNS=127.0.0.1:5354\nDomains=~example.dev\n",
	})
	for _, args := range [][]string{{"dns", "setup", "example.dev"}, {"dns", "setup", "example.dev", "--print"}} {
		code, stdout, stderr := runCLI(t, args...)
		if code != 1 || stdout != "" || len(*ran) != 0 ||
			!strings.Contains(stderr, "/etc/systemd/resolved.conf.d/eluve-dev.conf already routes ~example.dev") {
			t.Errorf("%v: exit %d, stdout %q, ran %q, stderr %q", args, code, stdout, stepLines(*ran), stderr)
		}
	}
}

func TestDNSSetupAlreadyDone(t *testing.T) {
	_, ran := fakeDNS(t, map[string]string{"devingress-example.dev.conf": plainDropIn})
	code, stdout, stderr := runCLI(t, "dns", "setup", "example.dev")
	if code != 0 || stderr != "" || len(*ran) != 0 || !strings.HasPrefix(stdout, dropInPath+" already routes example.dev") {
		t.Errorf("exit %d, stderr %q, ran %q, stdout %q", code, stderr, stepLines(*ran), stdout)
	}
}

func TestDNSRemove(t *testing.T) {
	removeCommands := []string{"sudo rm -f " + dropInPath, restartCmd}
	_, ran := fakeDNS(t, map[string]string{"devingress-example.dev.conf": plainDropIn})

	code, stdout, stderr := runCLI(t, "dns", "remove", "example.dev", "--print")
	if want := strings.Join(removeCommands, "\n") + "\n"; code != 0 || stderr != "" || stdout != want || len(*ran) != 0 {
		t.Errorf("--print: exit %d, stderr %q, ran %q, stdout:\n%s", code, stderr, stepLines(*ran), stdout)
	}

	code, stdout, stderr = runCLI(t, "dns", "remove", "example.dev")
	if code != 0 || stderr != "" || !slices.Equal(stepLines(*ran), removeCommands) {
		t.Errorf("exit %d, stderr %q, ran %q", code, stderr, stepLines(*ran))
	}
	if want := "+ " + strings.Join(removeCommands, "\n+ ") + "\n"; stdout != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", stdout, want)
	}

	_, ran = fakeDNS(t, nil)
	code, stdout, _ = runCLI(t, "dns", "remove", "example.dev")
	if code != 0 || len(*ran) != 0 || stdout != dropInPath+" does not exist; nothing to remove.\n" {
		t.Errorf("nothing to remove: exit %d, ran %q, stdout %q", code, stepLines(*ran), stdout)
	}
}

func TestDNSNoResolved(t *testing.T) {
	_, ran := fakeDNS(t, nil)
	dnsSupported = func() error {
		return errors.New("devingress dns needs systemd-resolved, and /run/systemd/resolve does not exist")
	}
	for _, args := range [][]string{{"dns", "setup", "example.dev", "--print"}, {"dns", "remove", "example.dev"}} {
		code, stdout, stderr := runCLI(t, args...)
		if code != 1 || stdout != "" || len(*ran) != 0 || !strings.Contains(stderr, "systemd-resolved") ||
			!strings.Contains(stderr, "address=/example.dev/127.0.0.1") {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
	}
}

func TestDNSUsage(t *testing.T) {
	fakeDNS(t, nil)
	for args, want := range map[string]string{
		"dns":                   "expected setup or remove",
		"dns start example.dev": "expected setup or remove",
		"dns setup":             "expected one domain",
		"dns setup a.dev b.dev": "expected one domain",
		"dns remove example.dev --reset-global-dns": "--reset-global-dns only applies to setup",
		"dns setup ~example.dev":                    "is not a domain name",
	} {
		code, stdout, stderr := runCLI(t, strings.Fields(args)...)
		if code != 2 || stdout != "" || !strings.Contains(stderr, want) {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
	}
}

// fakeResolver is fakeDNS on macOS, with files as /etc/resolver's contents.
func fakeResolver(t *testing.T, files map[string]string) (ran *[]service.Step) {
	t.Helper()
	_, ran = fakeDNS(t, nil)
	goos, read := hostOS, dnsReadResolver
	t.Cleanup(func() { hostOS, dnsReadResolver = goos, read })
	hostOS = "darwin"
	dnsSupported = func() error { return errors.New("dns on darwin checked for systemd-resolved") }
	dnsReadResolver = func(path string) ([]byte, error) {
		if f, ok := files[path]; ok {
			return []byte(f), nil
		}
		return nil, os.ErrNotExist
	}
	return ran
}

const (
	resolverPath = "/etc/resolver/example.dev"
	resolverFile = "# Written by devingress: routes example.dev to serve's DNS listener.\nnameserver 127.0.0.1\nport 15353\n"
	flushCmd     = "sudo killall -HUP mDNSResponder"
)

func TestResolverSetup(t *testing.T) {
	ran := fakeResolver(t, nil)
	setup := []string{"sudo mkdir -p /etc/resolver", "sudo tee " + resolverPath, flushCmd}
	want := setup[0] + "\n" + setup[1] + " >/dev/null <<'EOF'\n" + resolverFile + "EOF\n" + flushCmd + "\n"
	code, stdout, stderr := runCLI(t, "dns", "setup", "Example.dev", "--print")
	if code != 0 || stderr != "" || stdout != want || len(*ran) != 0 {
		t.Errorf("--print: exit %d, stderr %q, ran %q, stdout:\n%s\nwant:\n%s", code, stderr, stepLines(*ran), stdout, want)
	}

	code, stdout, stderr = runCLI(t, "dns", "setup", "example.dev")
	if code != 0 || stderr != "" || !slices.Equal(stepLines(*ran), setup) || (*ran)[1].Stdin != resolverFile {
		t.Fatalf("exit %d, stderr %q, ran %+v", code, stderr, *ran)
	}
	if want := "+ " + strings.Join(setup, "\n+ ") + "\n"; stdout != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", stdout, want)
	}

	*ran = nil
	conf := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "devingress", "config.toml")
	if err := os.MkdirAll(filepath.Dir(conf), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conf, []byte(`dns_listen = "[::1]:5300"`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCLI(t, "dns", "setup", "example.dev"); code != 0 ||
		(*ran)[1].Stdin != "# Written by devingress: routes example.dev to serve's DNS listener.\nnameserver ::1\nport 5300\n" {
		t.Errorf("dns_listen [::1]:5300: exit %d, stderr %q, ran %+v", code, stderr, *ran)
	}
	*ran = nil
	if err := os.WriteFile(conf, []byte(`dns_listen = ""`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCLI(t, "dns", "setup", "example.dev", "--print"); code != 1 ||
		!strings.Contains(stderr, "dns_listen is empty") || len(*ran) != 0 {
		t.Errorf("dns_listen off: exit %d, stderr %q, ran %q", code, stderr, stepLines(*ran))
	}
}

func TestResolverSetupExisting(t *testing.T) {
	ran := fakeResolver(t, map[string]string{resolverPath: resolverFile})
	for _, args := range [][]string{{"dns", "setup", "example.dev"}, {"dns", "setup", "example.dev", "--print"}} {
		code, stdout, stderr := runCLI(t, args...)
		if code != 0 || stderr != "" || len(*ran) != 0 || !strings.Contains(stdout, resolverPath+" already routes example.dev; nothing to do.") {
			t.Errorf("%v, ours: exit %d, stderr %q, ran %q, stdout %q", args, code, stderr, stepLines(*ran), stdout)
		}
	}

	ran = fakeResolver(t, map[string]string{resolverPath: "nameserver 127.0.0.1\nport 5354\n"})
	for _, args := range [][]string{{"dns", "setup", "example.dev"}, {"dns", "setup", "example.dev", "--print"}} {
		code, stdout, stderr := runCLI(t, args...)
		if code != 1 || stdout != "" || len(*ran) != 0 ||
			stderr != "devingress: "+resolverPath+" already routes example.dev; remove that routing first, or keep using it\n" {
			t.Errorf("%v, foreign: exit %d, stdout %q, ran %q, stderr %q", args, code, stdout, stepLines(*ran), stderr)
		}
	}
	code, stdout, stderr := runCLI(t, "dns", "remove", "example.dev")
	if code != 1 || stdout != "" || len(*ran) != 0 || !strings.Contains(stderr, "not written by devingress") {
		t.Errorf("remove foreign: exit %d, stdout %q, ran %q, stderr %q", code, stdout, stepLines(*ran), stderr)
	}
}

func TestResolverRemove(t *testing.T) {
	remove := []string{"sudo rm -f " + resolverPath, flushCmd}
	ran := fakeResolver(t, map[string]string{resolverPath: resolverFile})
	code, stdout, stderr := runCLI(t, "dns", "remove", "example.dev", "--print")
	if code != 0 || stderr != "" || stdout != strings.Join(remove, "\n")+"\n" || len(*ran) != 0 {
		t.Errorf("--print: exit %d, stderr %q, ran %q, stdout:\n%s", code, stderr, stepLines(*ran), stdout)
	}
	code, stdout, stderr = runCLI(t, "dns", "remove", "example.dev")
	if code != 0 || stderr != "" || !slices.Equal(stepLines(*ran), remove) || stdout != "+ "+strings.Join(remove, "\n+ ")+"\n" {
		t.Errorf("exit %d, stderr %q, ran %q, stdout:\n%s", code, stderr, stepLines(*ran), stdout)
	}

	ran = fakeResolver(t, nil)
	code, stdout, _ = runCLI(t, "dns", "remove", "example.dev", "--print")
	if code != 0 || len(*ran) != 0 || stdout != "# "+resolverPath+" does not exist; nothing to remove.\n" {
		t.Errorf("nothing to remove: exit %d, ran %q, stdout %q", code, stepLines(*ran), stdout)
	}
}

func TestResolverRejectsResetGlobalDNS(t *testing.T) {
	ran := fakeResolver(t, nil)
	for _, args := range [][]string{{"dns", "setup", "example.dev", "--reset-global-dns"}, {"dns", "setup", "example.dev", "--reset-global-dns", "--print"}} {
		code, stdout, stderr := runCLI(t, args...)
		if code != 2 || stdout != "" || len(*ran) != 0 || !strings.Contains(stderr, "--reset-global-dns only applies to systemd-resolved") {
			t.Errorf("%v: exit %d, stdout %q, ran %q, stderr %q", args, code, stdout, stepLines(*ran), stderr)
		}
	}
}
