// Package probe checks whether anything accepts connections on a route's upstream, by dialing it
// once. It is how status and doctor tell a dead dev server from a devingress problem.
package probe

import (
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Result is one upstream's outcome.
type Result int

const (
	Unknown Result = iota // not an upstream probe can dial: a placeholder, a port range, a list
	Up                    // accepted a connection
	Down                  // refused it or did not answer within Timeout
)

// Timeout bounds each dial. All dials run in parallel, so it also bounds a whole probe.
const Timeout = 300 * time.Millisecond

// DialFunc is net.DialTimeout, injectable for tests.
type DialFunc func(network, address string, timeout time.Duration) (net.Conn, error)

// Target turns an upstream, as stored in a route and handed to Caddy's reverse_proxy, into the
// network and address to dial. ok is false when it cannot be interpreted.
func Target(upstream string) (network, address string, ok bool) {
	if upstream == "" || strings.ContainsAny(upstream, "{} \t\n") {
		return "", "", false
	}
	if path, isUnix := strings.CutPrefix(upstream, "unix/"); isUnix {
		return "unix", path, path != ""
	}
	defaultPort := ""
	rest := upstream
	if scheme, after, found := strings.Cut(upstream, "://"); found {
		switch scheme {
		case "http", "h2c":
			defaultPort = "80"
		case "https":
			defaultPort = "443"
		default:
			return "", "", false
		}
		rest = after
	}
	if strings.Contains(rest, "/") {
		return "", "", false
	}
	host, port, err := net.SplitHostPort(rest)
	if err != nil {
		if defaultPort == "" {
			return "", "", false
		}
		host, port = strings.TrimSuffix(strings.TrimPrefix(rest, "["), "]"), defaultPort
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", "", false
	}
	return "tcp", net.JoinHostPort(host, port), true
}

// All dials every distinct upstream once, concurrently, and returns each one's result.
func All(dial DialFunc, upstreams []string) map[string]Result {
	out := make(map[string]Result, len(upstreams))
	var dials []string
	for _, u := range upstreams {
		if _, seen := out[u]; seen {
			continue
		}
		out[u] = Unknown
		if _, _, ok := Target(u); ok {
			dials = append(dials, u)
		}
	}
	results := make([]Result, len(dials))
	var wg sync.WaitGroup
	for i, u := range dials {
		wg.Go(func() {
			network, address, _ := Target(u)
			results[i] = Down
			if c, err := dial(network, address, Timeout); err == nil {
				c.Close()
				results[i] = Up
			}
		})
	}
	wg.Wait()
	for i, u := range dials {
		out[u] = results[i]
	}
	return out
}
