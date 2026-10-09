// Package service renders the systemd system unit that runs devingress serve at boot, and the
// privileged commands that install and remove it.
package service

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

// UnitPath is where install writes the unit.
const UnitPath = "/etc/systemd/system/devingress.service"

// Account is the user the unit runs serve as.
type Account struct {
	Name, Group string
	UID, GID    string
	Home        string
}

// Target returns the user install acts for: the caller or, when the caller is root, SUDO_USER.
// It refuses root, which serve must not run as.
func Target(asRoot bool, getenv func(string) string) (Account, error) {
	u, err := user.Current()
	if asRoot {
		u, err = user.Lookup(cmp.Or(getenv("SUDO_USER"), "root"))
	}
	if err != nil {
		return Account{}, err
	}
	if u.Uid == "0" {
		return Account{}, errors.New(`the unit would run serve as root: run "devingress service" as ` +
			"your own user (it calls sudo itself), or under sudo from that user")
	}
	group := u.Gid
	if g, err := user.LookupGroupId(u.Gid); err == nil {
		group = g.Name
	}
	return Account{Name: u.Username, Group: group, UID: u.Uid, GID: u.Gid, Home: u.HomeDir}, nil
}

// xdgVars are passed to serve when set where install runs, so the service and the user's shell
// agree on config, state and certificates. XDG_RUNTIME_DIR is always /run/user/<uid>.
var xdgVars = []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_DATA_HOME"}

// Unit renders the system unit that runs exe's serve as a, with the bind capability.
func Unit(a Account, exe string, getenv func(string) string) string {
	env := fmt.Sprintf("Environment=XDG_RUNTIME_DIR=/run/user/%s\n", a.UID)
	for _, v := range xdgVars {
		if val := getenv(v); val != "" {
			env += fmt.Sprintf("Environment=%s=%s\n", v, val)
		}
	}
	return fmt.Sprintf(`[Unit]
Description=devingress local HTTPS ingress
Requires=user-runtime-dir@%[1]s.service
After=user-runtime-dir@%[1]s.service network-online.target docker.service
Wants=network-online.target

[Service]
User=%[2]s
Group=%[3]s
%[4]sExecStart=%[5]s serve
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
`, a.UID, a.Name, a.Group, env, exe)
}

// Step is one privileged command.
type Step struct {
	Args  []string
	Stdin string // fed to the command: the unit, for tee
}

func (s Step) String() string { return strings.Join(s.Args, " ") }

// Install writes unit, enables lingering for a so /run/user/<uid> exists at boot, and starts the unit.
func Install(a Account, unit string) []Step {
	return []Step{
		{Args: []string{"sudo", "tee", UnitPath}, Stdin: unit},
		{Args: []string{"sudo", "loginctl", "enable-linger", a.Name}},
		{Args: []string{"sudo", "systemctl", "daemon-reload"}},
		{Args: []string{"sudo", "systemctl", "enable", "--now", "devingress"}},
	}
}

// Uninstall stops the unit and removes it. Lingering is left as it is.
func Uninstall() []Step {
	return []Step{
		{Args: []string{"sudo", "systemctl", "disable", "--now", "devingress"}},
		{Args: []string{"sudo", "rm", "-f", UnitPath}},
		{Args: []string{"sudo", "systemctl", "daemon-reload"}},
	}
}

// UserUnit is where the README's earlier setup put a's systemd user unit.
func UserUnit(a Account) string {
	return filepath.Join(a.Home, ".config", "systemd", "user", "devingress.service")
}

// Supported reports why this machine cannot run the unit, if it cannot.
func Supported() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("devingress service needs Linux with systemd, or macOS, not %s", runtime.GOOS)
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return errors.New("devingress service needs systemd, and /run/systemd/system does not exist")
	}
	return nil
}

// Exec runs s, with the terminal for sudo's password prompt.
func Exec(s Step, stdout io.Writer) error {
	cmd := exec.Command(s.Args[0], s.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, os.Stderr
	if s.Stdin != "" {
		cmd.Stdin, cmd.Stdout = strings.NewReader(s.Stdin), io.Discard // tee echoes what it writes
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", s, err)
	}
	return nil
}
