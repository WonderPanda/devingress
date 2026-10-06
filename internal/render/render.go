// Package render turns every live owner's routes into one Caddyfile and adapts
// it to the JSON config Caddy loads.
package render

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	_ "github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	_ "github.com/caddyserver/caddy/v2/modules/standard"

	"github.com/WonderPanda/porch/internal/routefile"
)

type Options struct {
	AdminSocket string
	StorageDir  string
	CARoot      string // dir holding rootCA.pem + rootCA-key.pem; "" lets Caddy mint its own root
	Bind        []string
	HTTPPort    int
	HTTPSPort   int
}

// Caddyfile renders the whole config. Two owners claiming the same host or
// path prefix is an error here: Caddy would reject the merged config, and
// with it every other owner's routes.
func Caddyfile(o Options, owners []routefile.Owner) (string, error) {
	if err := checkConflicts(owners); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("{\n")
	fmt.Fprintf(&b, "\tadmin unix/%s\n", o.AdminSocket)
	fmt.Fprintf(&b, "\tstorage file_system %s\n", o.StorageDir)
	b.WriteString("\tlocal_certs\n\tskip_install_trust\n")
	fmt.Fprintf(&b, "\thttp_port %d\n\thttps_port %d\n", o.HTTPPort, o.HTTPSPort)
	if len(o.Bind) > 0 {
		fmt.Fprintf(&b, "\tdefault_bind %s\n", strings.Join(o.Bind, " "))
	}
	// No UDP listener is opened, so never advertise h3: Safari caches the
	// alt-svc and then fails every wss:// upgrade.
	b.WriteString("\tservers {\n\t\tprotocols h1 h2\n\t}\n")
	if o.CARoot != "" {
		// mkcert's root is pathlen:0, so it cannot sign an intermediate. It
		// stands in as Caddy's intermediate too, signing leaves directly.
		ca := fmt.Sprintf("{\n\t\t\t\tcert %s/rootCA.pem\n\t\t\t\tkey %s/rootCA-key.pem\n\t\t\t}", o.CARoot, o.CARoot)
		fmt.Fprintf(&b, "\tpki {\n\t\tca local {\n\t\t\troot %s\n\t\t\tintermediate %s\n\t\t}\n\t}\n", ca, ca)
	}
	b.WriteString("}\n")

	var paths []routefile.PathRoute
	for _, ow := range owners {
		fmt.Fprintf(&b, "\n# owner: %s\n", ow.Dir)
		for _, s := range ow.Sites {
			writeSite(&b, s)
		}
		paths = append(paths, ow.Paths...)
	}
	if len(paths) > 0 {
		// Longest prefix first so a nested prefix is never shadowed.
		slices.SortFunc(paths, func(a, b routefile.PathRoute) int { return len(b.Prefix) - len(a.Prefix) })
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
		b.WriteString("}\n")
	}
	return b.String(), nil
}

func writeSite(b *strings.Builder, s routefile.Site) {
	fmt.Fprintf(b, "%s {\n", s.Host)
	if len(s.Protocols) > 0 {
		fmt.Fprintf(b, "\ttls internal {\n\t\talpn %s\n\t}\n", strings.Join(alpn(s.Protocols), " "))
	}
	if s.Caddy != "" {
		b.WriteString(s.Caddy)
		if !strings.HasSuffix(s.Caddy, "\n") {
			b.WriteString("\n")
		}
	} else {
		writeProxy(b, "\t", s.Upstream, s.Retry, s.Proxy)
	}
	b.WriteString("}\n")
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

// alpn maps porch's protocol names to ALPN identifiers.
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

func checkConflicts(owners []routefile.Owner) error {
	hosts, prefixes := map[string]string{}, map[string]string{}
	for _, o := range owners {
		for _, s := range o.Sites {
			if prev, ok := hosts[s.Host]; ok {
				return fmt.Errorf("host %s is claimed by both %s and %s", s.Host, prev, o.Dir)
			}
			hosts[s.Host] = o.Dir
		}
		for _, p := range o.Paths {
			if prev, ok := prefixes[p.Prefix]; ok {
				return fmt.Errorf("path %s is claimed by both %s and %s", p.Prefix, prev, o.Dir)
			}
			prefixes[p.Prefix] = o.Dir
		}
	}
	return nil
}

// Adapt converts the Caddyfile to JSON and provisions it without starting
// anything, so a bad config is rejected before it reaches the running server.
func Adapt(caddyfile string) ([]byte, error) {
	cfgJSON, _, err := caddyconfig.GetAdapter("caddyfile").Adapt([]byte(caddyfile), map[string]any{"filename": "porch"})
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
