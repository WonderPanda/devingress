// Package resolved renders the systemd-resolved drop-in that routes a domain to serve's DNS
// listener, the privileged commands that install and remove it, and reads what resolved already
// has configured.
package resolved

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/WonderPanda/devingress/internal/service"
)

// Dir holds resolved's drop-ins.
const Dir = "/etc/systemd/resolved.conf.d"

// RunDir exists while systemd-resolved runs.
const RunDir = "/run/systemd/resolve"

// DropInName is the file in Dir that routes domain to devingress.
func DropInName(domain string) string { return "devingress-" + domain + ".conf" }

// DropInPath is DropInName in Dir.
func DropInPath(domain string) string { return filepath.Join(Dir, DropInName(domain)) }

var domainRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

// Domain normalizes a domain from the command line: lower case, no trailing dot.
func Domain(arg string) (string, error) {
	d := strings.TrimSuffix(strings.ToLower(arg), ".")
	if !domainRE.MatchString(d) {
		return "", fmt.Errorf("%q is not a domain name", arg)
	}
	return d, nil
}

// DropIn routes ~domain to listen. With reset, it first clears the global DNS server list, so
// the servers already in it are not asked before listen.
func DropIn(listen, domain string, reset bool) string {
	clear := ""
	if reset {
		clear = fmt.Sprintf(`# Cleared: global DNS servers are asked before devingress for ~%s and
# answer NXDOMAIN for a real domain. Other lookups use per-link DNS.
DNS=
`, domain)
	}
	return fmt.Sprintf("[Resolve]\n%sDNS=%s\nDomains=~%s\n", clear, listen, domain)
}

// Setup writes domain's drop-in and restarts resolved.
func Setup(domain, dropIn string) []service.Step {
	return []service.Step{
		{Args: []string{"sudo", "mkdir", "-p", Dir}},
		{Args: []string{"sudo", "tee", DropInPath(domain)}, Stdin: dropIn},
		{Args: []string{"sudo", "systemctl", "restart", "systemd-resolved"}},
	}
}

// Remove deletes domain's drop-in and restarts resolved.
func Remove(domain string) []service.Step {
	return []service.Step{
		{Args: []string{"sudo", "rm", "-f", DropInPath(domain)}},
		{Args: []string{"sudo", "systemctl", "restart", "systemd-resolved"}},
	}
}

// RoutesDomain reports whether conf, a resolved.conf drop-in, has ~domain in a Domains= line.
func RoutesDomain(conf, domain string) bool {
	for line := range strings.SplitSeq(conf, "\n") {
		v, ok := strings.CutPrefix(strings.TrimSpace(line), "Domains=")
		if ok && slices.Contains(strings.Fields(v), "~"+domain) {
			return true
		}
	}
	return false
}

// GlobalServers parses the Global line of "resolvectl dns" output: the servers resolved asks
// for a routing domain in the global scope, in order.
func GlobalServers(resolvectlDNS string) ([]string, error) {
	for line := range strings.SplitSeq(resolvectlDNS, "\n") {
		if v, ok := strings.CutPrefix(line, "Global:"); ok {
			return strings.Fields(v), nil
		}
	}
	return nil, errors.New(`no "Global:" line in "resolvectl dns" output`)
}

// Supported reports why this machine cannot route a domain through resolved, if it cannot.
func Supported() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("devingress dns needs Linux with systemd-resolved, or macOS, not %s", runtime.GOOS)
	}
	if _, err := os.Stat(RunDir); err != nil {
		return errors.New("devingress dns needs systemd-resolved, and " + RunDir + " does not exist")
	}
	return nil
}
