// Package config holds devingress's host-level settings and filesystem locations.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/WonderPanda/devingress/internal/remote"
	"github.com/WonderPanda/devingress/internal/render"
)

type Config struct {
	Bind      []string `toml:"bind"`
	HTTPPort  int      `toml:"http_port"`
	HTTPSPort int      `toml:"https_port"`
	CARoot    string   `toml:"ca_root"`
	DNSListen string   `toml:"dns_listen"` // "" disables serve's DNS listener
	Remote    *Remote  `toml:"remote"`     // nil: remote mode is off

	// Warnings are problems Load found that do not stop devingress, for commands to print.
	Warnings []string `toml:"-"`
}

// Remote is config.toml's [remote] section: serve opted-in routes to the tailnet as
// <label>.<Suffix>, under one Let's Encrypt wildcard certificate obtained through Cloudflare DNS.
type Remote struct {
	Suffix      string `toml:"suffix"`
	DNSProvider string `toml:"dns_provider"`
	TokenFile   string `toml:"token_file"` // absolute once Load returns
}

// DefaultDNSListen is dns_listen when config.toml does not set it.
const DefaultDNSListen = "127.0.0.1:15353"

func xdg(env, fallback string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, fallback)
}

func ConfigFile() string {
	return filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), "devingress", "config.toml")
}
func StateDir() string { return filepath.Join(xdg("XDG_STATE_HOME", ".local/state"), "devingress") }
func DataDir() string  { return filepath.Join(xdg("XDG_DATA_HOME", ".local/share"), "devingress") }

func RuntimeDir() string {
	if v := os.Getenv("XDG_RUNTIME_DIR"); v != "" {
		return filepath.Join(v, "devingress")
	}
	return filepath.Join(StateDir(), "run")
}

func AdminSocket() string { return filepath.Join(RuntimeDir(), "admin.sock") }

// CaddyDir is Caddy's storage: certificates, and the root Caddy mints when
// there is no ca_root.
func CaddyDir() string { return filepath.Join(DataDir(), "caddy") }

// Load reads the optional config file and fills defaults. With a [remote] section, it fails
// unless the section is complete and its token file holds a well-formed token.
func Load() (Config, error) {
	var c Config
	md, err := toml.DecodeFile(ConfigFile(), &c)
	if err != nil && !os.IsNotExist(err) {
		return c, err
	}
	if c.Remote != nil {
		if err := c.Remote.check(filepath.Dir(ConfigFile())); err != nil {
			return c, fmt.Errorf("%s: [remote]: %w", ConfigFile(), err)
		}
		if w := remote.TokenModeWarning(c.Remote.TokenFile); w != "" {
			c.Warnings = append(c.Warnings, w)
		}
	}
	if !md.IsDefined("dns_listen") {
		c.DNSListen = DefaultDNSListen
	}
	if c.HTTPPort == 0 {
		c.HTTPPort = 80
	}
	if c.HTTPSPort == 0 {
		c.HTTPSPort = 443
	}
	if c.Bind == nil {
		c.Bind = defaultBind()
	}
	if c.CARoot == "" {
		c.CARoot = mkcertRoot()
	}
	return c, nil
}

// LoopbackNetwork is the Caddy network that serve registers on macOS to listen on loopback
// :80/:443 without root: macOS lets a non-root process bind a port below 1024 only on the
// wildcard address, so the network listens there, restricted to lo0.
const LoopbackNetwork = "devingress-lo0"

// goos is the OS Render renders for; tests replace it.
var goos = runtime.GOOS

func (c Config) Render() render.Options {
	o := render.Options{
		AdminSocket: AdminSocket(),
		StorageDir:  CaddyDir(),
		CARoot:      c.CARoot,
		Bind:        bindAddrs(goos, c.Bind),
		HTTPPort:    c.HTTPPort,
		HTTPSPort:   c.HTTPSPort,
	}
	if c.Remote != nil {
		o.Remote = &render.Remote{Suffix: c.Remote.Suffix, TokenFile: c.Remote.TokenFile}
	}
	return o
}

// Suffix is [remote]'s suffix, or "" when remote mode is off.
func (c Config) Suffix() string {
	if c.Remote == nil {
		return ""
	}
	return c.Remote.Suffix
}

// Redact removes the Cloudflare token from err's message. Caddy's errors can quote it, so
// every Caddy error devingress shows goes through here.
func (c Config) Redact(err error) error {
	if err == nil || c.Remote == nil {
		return err
	}
	data, rerr := os.ReadFile(c.Remote.TokenFile)
	if rerr != nil {
		return err
	}
	msg := err.Error()
	for _, t := range []string{string(data), strings.TrimSpace(string(data))} {
		msg = remote.Redact(msg, t)
	}
	if msg == err.Error() {
		return err
	}
	return errors.New(msg)
}

// domainRe is a DNS name of two or more labels.
var domainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// check validates r and resolves token_file: a leading ~ is the home directory, and a relative
// path is relative to dir, config.toml's directory.
func (r *Remote) check(dir string) error {
	r.Suffix = strings.ToLower(strings.Trim(r.Suffix, "."))
	switch {
	case r.Suffix == "":
		return errors.New(`suffix is required, e.g. suffix = "machine.example.dev"`)
	case !domainRe.MatchString(r.Suffix) || len(r.Suffix) > 253:
		return fmt.Errorf("suffix %q is not a DNS name of two or more labels", r.Suffix)
	}
	switch r.DNSProvider {
	case "":
		r.DNSProvider = "cloudflare"
	case "cloudflare":
	default:
		return fmt.Errorf("dns_provider %q is not supported: the only one is \"cloudflare\"", r.DNSProvider)
	}
	if r.TokenFile == "" {
		return errors.New(`token_file is required: the path of a file holding the Cloudflare API token, e.g. "~/.config/devingress/cloudflare-token"`)
	}
	path, err := expandHome(r.TokenFile)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	if path, err = filepath.Abs(path); err != nil {
		return err
	}
	if strings.ContainsAny(path, "{}\"\n") {
		return fmt.Errorf("token_file %q: the path cannot contain {, }, quotes or newlines (Caddy reads it through a {file.*} placeholder)", path)
	}
	r.TokenFile = path
	_, err = remote.ReadToken(path)
	return err
}

// expandHome replaces a leading "~" or "~/" with the home directory.
func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("token_file %s: %w", p, err)
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
}

// bindAddrs are bind's addresses as Caddy listens on them: on macOS, loopback addresses go
// through LoopbackNetwork. Everything else, and every address elsewhere, is as written.
func bindAddrs(goos string, bind []string) []string {
	if goos != "darwin" {
		return bind
	}
	out := make([]string, len(bind))
	for i, b := range bind {
		out[i] = b
		if ip := net.ParseIP(b); ip != nil && ip.IsLoopback() {
			out[i] = LoopbackNetwork + "/" + b
		}
	}
	return out
}

// RootCert is the absolute path of the root certificate serve signs with:
// rootCA.pem in ca_root (mkcert's CAROOT unless config.toml sets one), or else
// the root Caddy mints in CaddyDir, which may not exist yet. mkcert reports
// the former.
func (c Config) RootCert() (path string, mkcert bool, err error) {
	path, mkcert = filepath.Join(CaddyDir(), "pki", "authorities", "local", "root.crt"), false
	if c.CARoot != "" {
		path, mkcert = filepath.Join(c.CARoot, "rootCA.pem"), true
	}
	path, err = filepath.Abs(path)
	return path, mkcert, err
}

// defaultBind: loopback, plus the docker0 address when Docker is installed, so
// containers reach devingress through host.docker.internal (host-gateway) without
// exposing it on the LAN.
func defaultBind() []string {
	bind := []string{"127.0.0.1"}
	if iface, err := net.InterfaceByName("docker0"); err == nil {
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				bind = append(bind, ipn.IP.String())
			}
		}
	}
	return bind
}

// mkcertRoot reuses mkcert's root when one exists, so everything that already
// trusts it (browsers, NODE_EXTRA_CA_CERTS in containers) trusts devingress's certs.
func mkcertRoot() string {
	dir := os.Getenv("CAROOT")
	if dir == "" {
		if out, err := exec.Command("mkcert", "-CAROOT").Output(); err == nil {
			dir = strings.TrimSpace(string(out))
		}
	}
	if dir == "" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(dir, "rootCA-key.pem")); err != nil {
		return ""
	}
	return dir
}
