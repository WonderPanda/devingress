// Package config holds devingress's host-level settings and filesystem locations.
package config

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/WonderPanda/devingress/internal/render"
)

type Config struct {
	Bind      []string `toml:"bind"`
	HTTPPort  int      `toml:"http_port"`
	HTTPSPort int      `toml:"https_port"`
	CARoot    string   `toml:"ca_root"`
}

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

// Load reads the optional config file and fills defaults.
func Load() (Config, error) {
	var c Config
	if _, err := toml.DecodeFile(ConfigFile(), &c); err != nil && !os.IsNotExist(err) {
		return c, err
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

func (c Config) Render() render.Options {
	return render.Options{
		AdminSocket: AdminSocket(),
		StorageDir:  filepath.Join(DataDir(), "caddy"),
		CARoot:      c.CARoot,
		Bind:        c.Bind,
		HTTPPort:    c.HTTPPort,
		HTTPSPort:   c.HTTPSPort,
	}
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
