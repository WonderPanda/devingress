// Package remote is devingress's remote mode: the tailnet names ("twins") of the routes a route
// file opts in with remote = true, this machine's tailnet address, the Cloudflare API token, and
// the wildcard DNS record serve keeps pointed at that address.
package remote

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/WonderPanda/devingress/internal/routefile"
)

// labelRe is one DNS label: letters, digits and inner hyphens.
var labelRe = regexp.MustCompile(`(?i)^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// Name returns the remote twin of host, one of o's sites, under suffix: host with ".<domain>"
// replaced by ".<suffix>". name and skip are both "" when no twin is expected: remote mode is
// off (suffix is "") or o's route file does not set remote = true. Otherwise skip says why host
// gets no twin.
func Name(o routefile.Owner, host, suffix string) (name, skip string) {
	if suffix == "" || !o.Remote {
		return "", ""
	}
	if o.Domain == "" {
		return "", "the route file sets no domain, so there is no part of the host to replace with the suffix"
	}
	label, ok := strings.CutSuffix(host, "."+o.Domain)
	switch {
	case !ok:
		return "", fmt.Sprintf("the host is not under the route file's domain %s", o.Domain)
	case strings.Contains(label, "."):
		return "", fmt.Sprintf("%q is %d DNS labels, and the wildcard certificate *.%s covers one",
			label, strings.Count(label, ".")+1, suffix)
	case len(label) > 63:
		return "", fmt.Sprintf("%q is %d characters, and a DNS label holds at most 63", label, len(label))
	case !labelRe.MatchString(label):
		return "", fmt.Sprintf("%q is not a DNS label (letters, digits and inner hyphens)", label)
	}
	return label + "." + suffix, ""
}
