// Package macresolver renders the macOS resolver(5) file that routes a domain to serve's DNS
// listener, and the privileged commands that install and remove it.
package macresolver

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/WonderPanda/devingress/internal/service"
)

// Dir holds macOS's per-domain resolver files.
const Dir = "/etc/resolver"

// Marker starts every file devingress writes, so it can tell its own from someone else's.
const Marker = "# Written by devingress"

// Path is the file in Dir that routes domain.
func Path(domain string) string { return filepath.Join(Dir, domain) }

// File routes domain to listen, serve's dns_listen.
func File(listen, domain string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("dns_listen %q: %w", listen, err)
	}
	return fmt.Sprintf("%s: routes %s to serve's DNS listener.\nnameserver %s\nport %s\n", Marker, domain, host, port), nil
}

// Ours reports whether file, the content of a resolver file, was written by devingress.
func Ours(file string) bool { return strings.HasPrefix(file, Marker+":") }

// Domains lists the domains routed by the resolver files devingress wrote in dir (Dir, outside
// tests): each file is named for its domain.
func Domains(dir string) ([]string, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range paths {
		if data, err := os.ReadFile(p); err == nil && Ours(string(data)) {
			out = append(out, filepath.Base(p))
		}
	}
	return out, nil
}

// flush drops mDNSResponder's cached answers, the negative ones among them.
var flush = service.Step{Args: []string{"sudo", "killall", "-HUP", "mDNSResponder"}}

// Setup writes domain's resolver file.
func Setup(domain, file string) []service.Step {
	return []service.Step{
		{Args: []string{"sudo", "mkdir", "-p", Dir}},
		{Args: []string{"sudo", "tee", Path(domain)}, Stdin: file},
		flush,
	}
}

// Remove deletes domain's resolver file.
func Remove(domain string) []service.Step {
	return []service.Step{{Args: []string{"sudo", "rm", "-f", Path(domain)}}, flush}
}
