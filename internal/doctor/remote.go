package doctor

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/WonderPanda/devingress/internal/config"
	"github.com/WonderPanda/devingress/internal/remote"
	"github.com/WonderPanda/devingress/internal/routefile"
)

// ErrNoTailscale is TailscaleServeStatus's error when the tailscale CLI is not installed.
var ErrNoTailscale = errors.New("tailscale CLI not found")

// macTailscale is the CLI inside the macOS Tailscale app, which does not put it on PATH.
const macTailscale = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"

// tailscaleServeStatus runs "tailscale serve status --json", which only reads tailscaled's
// serve config: the CLI on PATH, else the one inside the macOS app.
func tailscaleServeStatus(goos string) ([]byte, error) {
	bin, err := exec.LookPath("tailscale")
	if err != nil {
		if _, statErr := os.Stat(macTailscale); goos != "darwin" || statErr != nil {
			return nil, ErrNoTailscale
		}
		bin = macTailscale
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "serve", "status", "--json").Output()
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return nil, fmt.Errorf("%s serve status --json: %s", bin, strings.TrimSpace(string(ee.Stderr)))
	}
	return out, err
}

// randomLabel is a DNS label no route claims, so a lookup or handshake for <label>.<suffix>
// exercises the wildcard alone.
func randomLabel() string {
	b := make([]byte, 4)
	rand.Read(b)
	return "devingress-doctor-" + hex.EncodeToString(b)
}

// checkRemote is every remote-mode check: the tailscale forward, the tailnet address, the
// wildcard DNS record, the wildcard certificate (on loopback, then through the forward), the
// Cloudflare token, and the hosts of remote = true route files that get no twin.
func checkRemote(deps Deps, cfg config.Config, owners []routefile.Owner) []Check {
	r := cfg.Remote
	out := []Check{checkTailscaleForward(deps, cfg.HTTPSPort)}
	tailnet, ipCheck := checkTailnetAddress(deps)
	out = append(out, ipCheck)
	out = append(out, checkWildcardDNS(deps, r.Suffix, tailnet))
	out = append(out, checkRemoteCert(deps, r.Suffix, net.JoinHostPort("127.0.0.1", fmt.Sprint(cfg.HTTPSPort)), "certificate",
		"serve obtains *."+r.Suffix+" from Let's Encrypt at start: check serve's log for the ACME error "+
			"(the token needs Zone:Read and DNS:Edit on the zone)"))
	if tailnet.IsValid() {
		out = append(out, checkRemoteCert(deps, r.Suffix, net.JoinHostPort(tailnet.String(), "443"), "end to end",
			"if the certificate check passes, the tailnet's :443 does not reach serve: "+forwardCommand(cfg.HTTPSPort)))
	}
	if w := remote.TokenModeWarning(r.TokenFile); w != "" {
		out = append(out, Check{Check: "token", Subject: r.TokenFile, Status: Warn, Message: w, Fix: "chmod 600 " + r.TokenFile})
	}
	out = append(out, checkToken(deps, r.TokenFile))
	return append(out, checkTwins(owners, r.Suffix)...)
}

// serveStatus is the part of tailscale's ipn.ServeConfig (ipn/serve.go) the forward check reads:
// TCP maps a port of the node's tailnet addresses to its handler.
type serveStatus struct {
	TCP map[string]struct {
		HTTPS         bool
		HTTP          bool
		TCPForward    string
		TerminateTLS  string
		ProxyProtocol int
	}
}

func forwardCommand(httpsPort int) string {
	return fmt.Sprintf("tailscale serve --bg --tcp 443 tcp://127.0.0.1:%d (on Linux with sudo, or after "+
		"\"sudo tailscale set --operator=$USER\"; the macOS app needs no sudo)", httpsPort)
}

// checkTailscaleForward checks that tailscaled forwards raw TCP on the tailnet's :443 to serve's
// loopback https_port, so serve terminates TLS with the wildcard certificate.
func checkTailscaleForward(deps Deps, httpsPort int) Check {
	const subject = "tailscale serve, tailnet port 443"
	want := net.JoinHostPort("127.0.0.1", fmt.Sprint(httpsPort))
	fix := forwardCommand(httpsPort)
	data, err := deps.TailscaleServeStatus()
	switch {
	case errors.Is(err, ErrNoTailscale):
		return Check{Check: "tailscale", Subject: subject, Status: Warn,
			Message: "no tailscale CLI on PATH (or at " + macTailscale + " on macOS), so the forward is not checked",
			Fix:     "install Tailscale, then " + fix}
	case err != nil:
		return Check{Check: "tailscale", Subject: subject, Status: Fail, Message: err.Error(), Fix: "start Tailscale and log in"}
	}
	var st serveStatus
	if err := json.Unmarshal(data, &st); err != nil {
		return Check{Check: "tailscale", Subject: subject, Status: Fail,
			Message: "tailscale serve status --json printed something that is not its JSON: " + err.Error(), Fix: fix}
	}
	h, ok := st.TCP["443"]
	var msg string
	switch {
	case !ok:
		msg = "nothing forwards the tailnet's port 443"
	case h.HTTPS || h.HTTP:
		msg = "port 443 is Tailscale's own HTTP(S) serve, not a TCP forward to devingress"
	case h.TCPForward != want:
		msg = fmt.Sprintf("port 443 forwards to %q, not %s (https_port)", h.TCPForward, want)
	case h.TerminateTLS != "":
		msg = fmt.Sprintf("Tailscale terminates TLS for %s before forwarding, but devingress must: forward raw TCP", h.TerminateTLS)
	case h.ProxyProtocol != 0:
		msg = "the forward sends a PROXY protocol header, which devingress does not expect"
	default:
		return Check{Check: "tailscale", Subject: subject, Status: OK, Message: "forwards raw TCP to " + want}
	}
	return Check{Check: "tailscale", Subject: subject, Status: Fail, Message: msg, Fix: fix}
}

func checkTailnetAddress(deps Deps) (netip.Addr, Check) {
	const subject = "tailnet address"
	ifaces, err := deps.Interfaces()
	if err != nil {
		return netip.Addr{}, Check{Check: "tailnet", Subject: subject, Status: Fail, Message: err.Error()}
	}
	ip, err := remote.TailnetIPv4(ifaces, deps.GOOS)
	if err != nil {
		return netip.Addr{}, Check{Check: "tailnet", Subject: subject, Status: Fail,
			Message: err.Error() + "; serve cannot point the wildcard DNS record at this machine",
			Fix:     "start Tailscale and log in (tailscale up)"}
	}
	return ip, Check{Check: "tailnet", Subject: subject, Status: OK, Message: ip.String()}
}

// checkWildcardDNS resolves a name no twin claims, so only the *.<suffix> record answers it.
func checkWildcardDNS(deps Deps, suffix string, tailnet netip.Addr) Check {
	name := deps.Label() + "." + suffix
	subject := "*." + suffix + " DNS"
	fix := "serve sets *." + suffix + " (DNS-only) to the tailnet address at start and every " +
		remote.Interval.String() + ": check the \"remote:\" lines of serve's log, and the token check below"
	ips, err := deps.Lookup(name)
	if err != nil {
		return Check{Check: "wildcard", Subject: subject, Status: Fail, Message: name + " does not resolve: " + err.Error(), Fix: fix}
	}
	if !tailnet.IsValid() {
		return Check{Check: "wildcard", Subject: subject, Status: Warn,
			Message: fmt.Sprintf("%s resolves to %v; without a tailnet address there is nothing to compare it with", name, ips),
			Fix:     "start Tailscale and log in (tailscale up)"}
	}
	if slices.ContainsFunc(ips, func(ip net.IP) bool { a, _ := netip.AddrFromSlice(ip); return a.Unmap() == tailnet }) {
		return Check{Check: "wildcard", Subject: subject, Status: OK, Message: fmt.Sprintf("%s resolves to %s, this machine's tailnet address", name, tailnet)}
	}
	if slices.ContainsFunc(ips, net.IP.IsLoopback) {
		for d := suffix; d != ""; {
			if p, ok := routedDomains(deps)[d]; ok {
				return Check{Check: "wildcard", Subject: subject, Status: Warn,
					Message: fmt.Sprintf("%s resolves to loopback here because %s routes %s to serve's DNS listener; "+
						"other devices on the tailnet ask public DNS, which this machine cannot see", name, p, d),
					Fix: "from another device on the tailnet: dig +short " + name + " (want " + tailnet.String() + ")"}
			}
			_, d, _ = strings.Cut(d, ".")
		}
	}
	return Check{Check: "wildcard", Subject: subject, Status: Fail,
		Message: fmt.Sprintf("%s resolves to %v, not %s, this machine's tailnet address", name, ips, tailnet), Fix: fix}
}

// checkRemoteCert handshakes with addr for a name no twin claims and verifies the certificate
// against the system roots, not the local CA: what a phone on the tailnet sees. check names it:
// "certificate" on loopback, "end to end" through the tailnet address and Tailscale's forward.
func checkRemoteCert(deps Deps, suffix, addr, check, fix string) Check {
	sni := deps.Label() + "." + suffix
	subject := fmt.Sprintf("*.%s at %s", suffix, addr)
	roots, err := deps.CertPool()
	if err != nil {
		return Check{Check: check, Subject: subject, Status: Fail, Message: "system roots: " + err.Error()}
	}
	conn, err := deps.DialTLS("tcp", addr, &tls.Config{ServerName: sni, RootCAs: roots})
	if err != nil {
		return Check{Check: check, Subject: subject, Status: Fail,
			Message: fmt.Sprintf("TLS handshake for %s does not verify against the system roots: %v", sni, err), Fix: fix}
	}
	leaf := conn.ConnectionState().PeerCertificates[0]
	conn.Close()
	left := leaf.NotAfter.Sub(deps.Now())
	if left < 14*24*time.Hour {
		return Check{Check: check, Subject: subject, Status: Warn,
			Message: fmt.Sprintf("expires %s, in %d days: Caddy renews 30 days ahead, so renewal is failing", leaf.NotAfter.Format(time.DateOnly), int(left.Hours()/24)),
			Fix:     "check serve's log for the ACME error, and the token check"}
	}
	return Check{Check: check, Subject: subject, Status: OK,
		Message: fmt.Sprintf("publicly trusted, expires %s", leaf.NotAfter.Format(time.DateOnly))}
}

// tokenVerifyURL is Cloudflare's endpoint that reports a user API token's status.
const tokenVerifyURL = "https://api.cloudflare.com/client/v4/user/tokens/verify"

// checkToken asks Cloudflare whether the token is active, and warns as its expiry nears: one
// token shared by every machine stops renewals on all of them at once.
func checkToken(deps Deps, path string) Check {
	token, err := remote.ReadToken(path)
	if err != nil {
		return Check{Check: "token", Subject: path, Status: Fail, Message: err.Error()}
	}
	redact := func(s string) string { return remote.Redact(s, token) }
	if strings.HasPrefix(token, "cfat_") {
		return Check{Check: "token", Subject: path, Status: Warn,
			Message: "an account API token: Cloudflare verifies those per account, so it is not checked here",
			Fix:     "check its status and expiry in the Cloudflare dashboard"}
	}
	fix := "create a Cloudflare API token with Zone:Read and DNS:Edit on the zone and save it to " + path + " (mode 600)"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenVerifyURL, nil)
	if err != nil {
		return Check{Check: "token", Subject: path, Status: Fail, Message: redact(err.Error())}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := deps.HTTPDo(req)
	if err != nil {
		return Check{Check: "token", Subject: path, Status: Fail, Message: "cannot reach Cloudflare: " + redact(err.Error())}
	}
	defer resp.Body.Close()
	var body struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
		Result struct {
			Status    string `json:"status"`
			ExpiresOn string `json:"expires_on"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Check{Check: "token", Subject: path, Status: Fail, Message: fmt.Sprintf("Cloudflare answered %s with no JSON", resp.Status)}
	}
	if resp.StatusCode != http.StatusOK || body.Result.Status != "active" {
		var msgs []string
		for _, e := range body.Errors {
			msgs = append(msgs, e.Message)
		}
		msg := fmt.Sprintf("Cloudflare answered %s: status %q", resp.Status, body.Result.Status)
		if len(msgs) > 0 {
			msg += ": " + strings.Join(msgs, "; ")
		}
		return Check{Check: "token", Subject: path, Status: Fail, Message: redact(msg), Fix: fix}
	}
	if body.Result.ExpiresOn == "" {
		return Check{Check: "token", Subject: path, Status: OK, Message: "active, no expiry"}
	}
	expires, err := time.Parse(time.RFC3339, body.Result.ExpiresOn)
	if err != nil {
		return Check{Check: "token", Subject: path, Status: Warn, Message: "active, with an expiry that does not parse: " + redact(body.Result.ExpiresOn)}
	}
	if left := expires.Sub(deps.Now()); left < 30*24*time.Hour {
		return Check{Check: "token", Subject: path, Status: Warn,
			Message: fmt.Sprintf("active, but expires %s, in %d days; certificate renewals on every machine sharing it stop then",
				expires.Format(time.DateOnly), int(left.Hours()/24)),
			Fix: "roll the token in the Cloudflare dashboard and copy it to " + path + " on each machine with [remote]"}
	}
	return Check{Check: "token", Subject: path, Status: OK, Message: "active, expires " + expires.Format(time.DateOnly)}
}

// checkTwins warns about each host of a live remote = true owner that gets no twin.
func checkTwins(owners []routefile.Owner, suffix string) []Check {
	var out []Check
	for _, o := range owners {
		if _, err := os.Stat(o.Dir); err != nil {
			continue // covered by the owners check
		}
		for _, s := range o.Sites {
			if _, skip := remote.Name(o, s.Host, suffix); skip != "" {
				out = append(out, Check{Check: "remote", Subject: s.Host, Status: Warn, Message: "no remote URL: " + skip,
					Fix: "in " + o.File + " of " + o.Label() + ", set domain and give the route a one-label host (e.g. web-${slug}), then devingress up"})
			}
		}
	}
	return out
}
