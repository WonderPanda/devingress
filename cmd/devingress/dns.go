package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/WonderPanda/devingress/internal/config"
	"github.com/WonderPanda/devingress/internal/macresolver"
	"github.com/WonderPanda/devingress/internal/resolved"
	"github.com/WonderPanda/devingress/internal/service"
)

const dnsHelp = `Route a domain's lookups to serve's DNS listener, so every name under it resolves to loopback
(A 127.0.0.1, AAAA ::1) with no per-host entries: through systemd-resolved on Linux, through
/etc/resolver on macOS. Privileges are needed once, here; up and down never touch DNS. serve
listens on dns_listen in config.toml (default 127.0.0.1:15353; "" turns it off).

On Linux, setup writes /etc/systemd/resolved.conf.d/devingress-<domain>.conf:

  [Resolve]
  DNS=<dns_listen>
  Domains=~<domain>

and restarts systemd-resolved. It does nothing, and exits 1, when another drop-in in that
directory already routes ~<domain> (an existing dnsmasq setup, say), or when resolved has global
DNS servers ("resolvectl dns", the Global line): a drop-in's DNS= adds dns_listen after them, and
resolved asks a global-scope routing domain's servers in that order, so a global server answers
first (NXDOMAIN) and the query never reaches devingress. --reset-global-dns clears the global list
in the drop-in instead; that affects every lookup on the machine: names outside <domain> then
resolve only through per-link DNS (e.g. from DHCP). When the drop-in already exists, setup changes
nothing. remove deletes the drop-in and restarts systemd-resolved.

On macOS, setup writes /etc/resolver/<domain>:

  # Written by devingress: routes <domain> to serve's DNS listener.
  nameserver <dns_listen's host>
  port <dns_listen's port>

and flushes mDNSResponder's cache ("killall -HUP mDNSResponder"). A per-domain resolver wins for
its domain, so there are no global servers to clear, and --reset-global-dns is a usage error. When
the file already exists, setup changes nothing if devingress wrote it, and exits 1 otherwise.
remove deletes the file, only if devingress wrote it, and flushes the cache.

Run it as your own user: it calls sudo for each privileged command, and prints each one before it
runs. Containers do not use the host's resolver; give them extra_hosts.

Flags:
  --reset-global-dns    setup, Linux: clear resolved's global DNS servers in the drop-in
  --print               print the file and the commands as a shell script; change nothing.
                        The checks above still run, so it refuses whatever a real run refuses.

Exit codes:
  0  set up, removed, already in place, or printed
  1  no systemd-resolved on Linux, the domain is already routed elsewhere, global DNS servers
     without --reset-global-dns, or a command failed
  2  bad flag or argument

Examples:
  devingress dns setup example.dev --print
  devingress dns setup example.dev
  devingress dns setup example.dev --reset-global-dns
  devingress dns remove example.dev
`

// What dns touches on this machine; tests replace them.
var (
	dnsSupported = resolved.Supported
	// dnsConfs reads every *.conf in resolved.Dir, by file name.
	dnsConfs = func() (map[string]string, error) {
		paths, err := filepath.Glob(filepath.Join(resolved.Dir, "*.conf"))
		if err != nil {
			return nil, err
		}
		confs := map[string]string{}
		for _, p := range paths {
			data, err := os.ReadFile(p)
			if err != nil {
				return nil, err
			}
			confs[filepath.Base(p)] = string(data)
		}
		return confs, nil
	}
	// dnsGlobalServers runs the read-only "resolvectl dns".
	dnsGlobalServers = func() ([]string, error) {
		out, err := exec.Command("resolvectl", "dns").Output()
		if err != nil {
			return nil, fmt.Errorf("resolvectl dns: %w", err)
		}
		return resolved.GlobalServers(string(out))
	}
	dnsRun = service.Exec
	// dnsReadResolver reads a file in macresolver.Dir.
	dnsReadResolver = os.ReadFile
)

func dnsCmd(args []string, stdout io.Writer) error {
	fs := flags("dns")
	printOnly := fs.Bool("print", false, "print the drop-in and commands instead of running them")
	reset := fs.Bool("reset-global-dns", false, "clear resolved's global DNS servers in the drop-in")
	pos, err := positional(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 || pos[0] != "setup" && pos[0] != "remove" {
		return usageError{fmt.Errorf("expected setup or remove, got %q", pos)}
	}
	if len(pos) != 2 {
		return usageError{fmt.Errorf("expected one domain, got %q", pos[1:])}
	}
	if *reset && pos[0] != "setup" {
		return usageError{errors.New("--reset-global-dns only applies to setup")}
	}
	if *reset && hostOS == "darwin" {
		return usageError{errors.New("--reset-global-dns only applies to systemd-resolved; " +
			"macOS asks /etc/resolver/<domain> before any global server")}
	}
	domain, err := resolved.Domain(pos[1])
	if err != nil {
		return usageError{err}
	}
	if hostOS == "darwin" {
		return resolverCmd(pos[0], domain, *printOnly, stdout)
	}
	if err := dnsSupported(); err != nil {
		return fmt.Errorf("%w\nRoute %s with dnsmasq instead, e.g. in /etc/dnsmasq.d/devingress.conf:\n"+
			"  address=/%[2]s/127.0.0.1", err, domain)
	}
	confs, err := dnsConfs()
	if err != nil {
		return err
	}
	ours := resolved.DropInName(domain)
	_, exists := confs[ours]

	if pos[0] == "remove" {
		if !exists {
			note(stdout, *printOnly, resolved.DropInPath(domain)+" does not exist; nothing to remove.")
			return nil
		}
		return runSteps(resolved.Remove(domain), *printOnly, dnsRun, stdout)
	}

	if exists {
		note(stdout, *printOnly, fmt.Sprintf("%s already routes %s; nothing to do. "+
			`"devingress dns remove %[2]s" first to change it.`, resolved.DropInPath(domain), domain))
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(confs)) {
		if resolved.RoutesDomain(confs[name], domain) {
			return fmt.Errorf("%s already routes ~%s; remove that routing first, or keep using it",
				filepath.Join(resolved.Dir, name), domain)
		}
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.DNSListen == "" {
		return fmt.Errorf("dns_listen is empty in %s, so serve answers no DNS; set it (default %q)",
			config.ConfigFile(), config.DefaultDNSListen)
	}
	servers, err := dnsGlobalServers()
	if err != nil {
		return err
	}
	// Another domain's devingress drop-in puts dns_listen in the global list; that one answers.
	servers = slices.DeleteFunc(servers, func(s string) bool { return s == cfg.DNSListen })
	if len(servers) > 0 && !*reset {
		return fmt.Errorf("systemd-resolved has global DNS servers: %s\n"+
			"A drop-in's DNS= adds %s after them, and resolved asks them in that order\n"+
			"for ~%s: they answer first (NXDOMAIN) and the query never reaches devingress.\n"+
			"Rerun with --reset-global-dns to clear them; that affects every lookup on this machine",
			strings.Join(servers, " "), cfg.DNSListen, domain)
	}
	if len(servers) > 0 {
		note(stdout, *printOnly, fmt.Sprintf("Warning: --reset-global-dns removes %s from resolved's global scope.\n"+
			"That affects every lookup on this machine, not just %s: other names then resolve only\n"+
			"through per-link DNS (e.g. from DHCP).", strings.Join(servers, " "), domain))
	}
	return runSteps(resolved.Setup(domain, resolved.DropIn(cfg.DNSListen, domain, *reset)), *printOnly, dnsRun, stdout)
}

// resolverCmd sets up or removes domain's /etc/resolver file: dns on darwin.
func resolverCmd(sub, domain string, printOnly bool, stdout io.Writer) error {
	path := macresolver.Path(domain)
	data, err := dnsReadResolver(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	exists, ours := err == nil, err == nil && macresolver.Ours(string(data))

	if sub == "remove" {
		switch {
		case !exists:
			note(stdout, printOnly, path+" does not exist; nothing to remove.")
			return nil
		case !ours:
			return fmt.Errorf("%s was not written by devingress; remove it yourself if you mean to", path)
		}
		return runSteps(macresolver.Remove(domain), printOnly, dnsRun, stdout)
	}

	if ours {
		note(stdout, printOnly, fmt.Sprintf("%s already routes %s; nothing to do. "+
			`"devingress dns remove %[2]s" first to change it.`, path, domain))
		return nil
	}
	if exists {
		return fmt.Errorf("%s already routes %s; remove that routing first, or keep using it", path, domain)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.DNSListen == "" {
		return fmt.Errorf("dns_listen is empty in %s, so serve answers no DNS; set it (default %q)",
			config.ConfigFile(), config.DefaultDNSListen)
	}
	file, err := macresolver.File(cfg.DNSListen, domain)
	if err != nil {
		return err
	}
	return runSteps(macresolver.Setup(domain, file), printOnly, dnsRun, stdout)
}
