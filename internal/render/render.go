// Package render turns every live owner's routes into one Caddyfile and adapts
// it to the JSON config Caddy loads.
package render

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	_ "github.com/caddy-dns/cloudflare" // dns.providers.cloudflare, for remote mode's DNS-01 challenge
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	_ "github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	_ "github.com/caddyserver/caddy/v2/modules/standard"

	"github.com/WonderPanda/devingress/internal/remote"
	"github.com/WonderPanda/devingress/internal/routefile"
)

type Options struct {
	AdminSocket string
	StorageDir  string
	CARoot      string // dir holding rootCA.pem + rootCA-key.pem; "" lets Caddy mint its own root
	Bind        []string
	HTTPPort    int
	HTTPSPort   int
	Remote      *Remote // nil: remote mode is off
}

// Remote is what remote mode renders from: twins of opted-in sites under Suffix, and the one
// *.<Suffix> certificate, issued by Let's Encrypt through a Cloudflare DNS-01 challenge with the
// token in TokenFile (an absolute path; only the path is rendered, never the token).
type Remote struct {
	Suffix    string
	TokenFile string
}

func (o Options) suffix() string {
	if o.Remote == nil {
		return ""
	}
	return o.Remote.Suffix
}

// Caddyfile renders the whole config. Two owners claiming the same host or
// path prefix is an error here: Caddy would reject the merged config, and
// with it every other owner's routes. In remote mode a twin is a host too.
func Caddyfile(o Options, owners []routefile.Owner) (string, error) {
	if err := checkConflicts(owners, o.suffix()); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("{\n")
	fmt.Fprintf(&b, "\tadmin %s\n", quotePath("unix/"+o.AdminSocket))
	fmt.Fprintf(&b, "\tstorage file_system %s\n", quotePath(o.StorageDir))
	b.WriteString("\tlocal_certs\n\tskip_install_trust\n")
	fmt.Fprintf(&b, "\thttp_port %d\n\thttps_port %d\n", o.HTTPPort, o.HTTPSPort)
	if len(o.Bind) > 0 {
		fmt.Fprintf(&b, "\tdefault_bind %s\n", strings.Join(o.Bind, " "))
	}
	// No UDP listener is opened, so never advertise h3: Safari caches the
	// alt-svc and then fails every wss:// upgrade.
	b.WriteString("\tservers {\n\t\tprotocols h1 h2\n")
	if o.Remote != nil {
		// Every twin shares the *.<suffix> certificate, so a browser may send a request for one
		// twin over a connection made for another, skipping the ALPN pin of a protocols = ["h1"]
		// twin. strict_sni_host answers such a request 421, and the browser reconnects.
		b.WriteString("\t\tstrict_sni_host on\n")
	}
	b.WriteString("\t}\n")
	if o.CARoot != "" {
		// mkcert's root is pathlen:0, so it cannot sign an intermediate. It
		// stands in as Caddy's intermediate too, signing leaves directly.
		ca := fmt.Sprintf("{\n\t\t\t\tcert %s\n\t\t\t\tkey %s\n\t\t\t}",
			quotePath(o.CARoot+"/rootCA.pem"), quotePath(o.CARoot+"/rootCA-key.pem"))
		fmt.Fprintf(&b, "\tpki {\n\t\tca local {\n\t\t\troot %s\n\t\t\tintermediate %s\n\t\t}\n\t}\n", ca, ca)
	}
	b.WriteString("}\n")

	type ownedPath struct {
		routefile.PathRoute
		Owner string
	}
	var paths []ownedPath
	for _, ow := range owners {
		fmt.Fprintf(&b, "\n# owner: %s\n", ow.Label())
		for _, s := range ow.Sites {
			writeSite(&b, s, ow.Label(), false)
		}
		for _, s := range ow.Sites {
			if twin, _ := remote.Name(ow, s.Host, o.suffix()); twin != "" {
				s.Host = twin
				writeSite(&b, s, ow.Label(), true)
			}
		}
		for _, p := range ow.Paths {
			paths = append(paths, ownedPath{p, ow.Label()})
		}
	}
	if o.Remote != nil {
		writeWildcard(&b, *o.Remote)
	}
	if len(paths) > 0 {
		// Longest prefix first so a nested prefix is never shadowed.
		slices.SortFunc(paths, func(a, b ownedPath) int { return len(b.Prefix) - len(a.Prefix) })
		fmt.Fprintf(&b, "\nhttp://:%d {\n", o.HTTPPort)
		for _, p := range paths {
			dir := "handle"
			if p.Strip {
				dir = "handle_path"
			}
			fmt.Fprintf(&b, "\t%s %s/* {\n", dir, p.Prefix)
			writeProxy(&b, "\t\t", p.Upstream, "", p.Proxy)
			b.WriteString("\t}\n")
		}
		// handle_errors is site-level, so one block holds every prefix. It
		// matches the original path, even for a handle_path route.
		b.WriteString("\thandle_errors 502 503 504 {\n")
		for _, p := range paths {
			fmt.Fprintf(&b, "\t\thandle %s/* {\n", p.Prefix)
			writeUnreachable(&b, "\t\t\t", p.Upstream, p.Prefix+"/*", p.Owner)
			b.WriteString("\t\t}\n")
		}
		b.WriteString("\t}\n")
		b.WriteString("}\n")
	}
	return b.String(), nil
}

// writeSite writes one site block. A remote twin names no issuer: the *.<suffix> site's DNS-01
// policy covers it, and Caddy serves it the wildcard certificate instead of obtaining its own.
func writeSite(b *strings.Builder, s routefile.Site, owner string, twin bool) {
	fmt.Fprintf(b, "%s {\n", s.Host)
	if len(s.Protocols) > 0 {
		issuer := " internal"
		if twin {
			issuer = ""
		}
		fmt.Fprintf(b, "\ttls%s {\n\t\talpn %s\n\t}\n", issuer, strings.Join(alpn(s.Protocols), " "))
	}
	if s.Caddy != "" {
		b.WriteString(s.Caddy)
		if !strings.HasSuffix(s.Caddy, "\n") {
			b.WriteString("\n")
		}
	} else {
		writeProxy(b, "\t", s.Upstream, s.Retry, s.Proxy)
		b.WriteString("\thandle_errors 502 503 504 {\n")
		writeUnreachable(b, "\t\t", s.Upstream, s.Host, owner)
		b.WriteString("\t}\n")
	}
	b.WriteString("}\n")
}

// writeWildcard writes the *.<suffix> site: it carries the one public certificate every twin is
// served, from Let's Encrypt through a Cloudflare DNS-01 challenge (never a certificate per
// host, which Certificate Transparency logs would publish with every branch name), and answers
// 404 for a name no twin claims. The token reaches Caddy as a {file.*} placeholder, which Caddy
// reads when it provisions the DNS provider, so neither the Caddyfile nor the JSON holds it.
func writeWildcard(b *strings.Builder, r Remote) {
	fmt.Fprintf(b, "\n# remote: every twin is served the *.%s certificate\n", r.Suffix)
	fmt.Fprintf(b, "*.%s {\n", r.Suffix)
	fmt.Fprintf(b, "\ttls {\n\t\tdns cloudflare %s\n\t}\n", quotePath("{file."+r.TokenFile+"}"))
	b.WriteString("\theader Content-Type \"text/plain; charset=utf-8\"\n")
	b.WriteString("\trespond \"devingress: no remote route is registered for {http.request.host}. " +
		"Run \\\"devingress status\\\" on this machine.\" 404\n")
	b.WriteString("}\n")
}

// writeUnreachable answers a proxy error with a plain-text body naming the
// upstream, what it serves and its owner, keeping Caddy's status.
func writeUnreachable(b *strings.Builder, indent, upstream, what, owner string) {
	msg := fmt.Sprintf("devingress: nothing answered at %s for %s (owner %s). Start its dev server, or run \"devingress status\".",
		upstream, what, owner)
	fmt.Fprintf(b, "%sheader Content-Type \"text/plain; charset=utf-8\"\n", indent)
	fmt.Fprintf(b, "%srespond \"%s\" {err.status_code}\n", indent, quoteLiteral.Replace(msg))
}

// quoteLiteral escapes text for a double-quoted Caddyfile token whose value
// is placeholder-expanded at request time, so it comes out as written.
var quoteLiteral = strings.NewReplacer(`"`, `\"`, "{", `\{`, "}", `\}`)

// quotePath makes a filesystem path one Caddyfile token, so a space (macOS's
// "Application Support") does not split it into several.
func quotePath(p string) string {
	return `"` + strings.ReplaceAll(p, `"`, `\"`) + `"`
}

func writeProxy(b *strings.Builder, indent, upstream, retry, raw string) {
	if retry == "" && raw == "" {
		fmt.Fprintf(b, "%sreverse_proxy %s\n", indent, upstream)
		return
	}
	fmt.Fprintf(b, "%sreverse_proxy %s {\n", indent, upstream)
	if retry != "" {
		// Dev servers refuse connections briefly while they restart or under a
		// burst; retry the refusal instead of returning 502.
		fmt.Fprintf(b, "%s\tlb_try_duration %s\n%s\tlb_try_interval 250ms\n", indent, retry, indent)
	}
	if raw != "" {
		for _, l := range strings.Split(strings.TrimRight(raw, "\n"), "\n") {
			fmt.Fprintf(b, "%s\t%s\n", indent, strings.TrimSpace(l))
		}
	}
	fmt.Fprintf(b, "%s}\n", indent)
}

// alpn maps devingress's protocol names to ALPN identifiers.
func alpn(protocols []string) []string {
	out := make([]string, 0, len(protocols))
	for _, p := range protocols {
		switch p {
		case "h1":
			out = append(out, "http/1.1")
		case "h2":
			out = append(out, "h2")
		default:
			out = append(out, p)
		}
	}
	return out
}

func checkConflicts(owners []routefile.Owner, suffix string) error {
	hosts, prefixes := map[string]string{}, map[string]string{}
	claim := func(host string, o routefile.Owner) error {
		if prev, ok := hosts[host]; ok {
			return fmt.Errorf("host %s is claimed by both %s and %s", host, prev, o.Label())
		}
		hosts[host] = o.Label()
		return nil
	}
	for _, o := range owners {
		for _, s := range o.Sites {
			if err := claim(s.Host, o); err != nil {
				return err
			}
		}
		for _, s := range o.Sites {
			if twin, _ := remote.Name(o, s.Host, suffix); twin != "" {
				if err := claim(twin, o); err != nil {
					return err
				}
			}
		}
		for _, p := range o.Paths {
			if prev, ok := prefixes[p.Prefix]; ok {
				return fmt.Errorf("path %s is claimed by both %s and %s", p.Prefix, prev, o.Label())
			}
			prefixes[p.Prefix] = o.Label()
		}
	}
	return nil
}

// Adapt converts the Caddyfile to JSON and provisions it without starting
// anything, so a bad config is rejected before it reaches the running server.
func Adapt(caddyfile string) ([]byte, error) {
	cfgJSON, _, err := caddyconfig.GetAdapter("caddyfile").Adapt([]byte(caddyfile), map[string]any{"filename": "devingress"})
	if err != nil {
		return nil, err
	}
	var cfg caddy.Config
	if err := json.Unmarshal(cfgJSON, &cfg); err != nil {
		return nil, err
	}
	// Provisioning logs at info; keep them out of the CLI's output. Only this
	// validation copy is changed, not the JSON the server loads.
	cfg.Logging = &caddy.Logging{Logs: map[string]*caddy.CustomLog{"default": {BaseLog: caddy.BaseLog{Level: "ERROR"}}}}
	if err := caddy.Validate(&cfg); err != nil {
		return nil, err
	}
	return cfgJSON, nil
}
