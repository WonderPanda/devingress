package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/WonderPanda/devingress/internal/launchd"
	"github.com/WonderPanda/devingress/internal/service"
)

const serviceHelp = `Install or remove what runs "devingress serve" in the background as you: a systemd system unit
on Linux, a LaunchAgent on macOS. Run it as your own user.

On Linux, systemd grants the unit the capability to bind ports below 1024, so there is no setcap to
lose when the binary is reinstalled. service calls sudo for each privileged command. Run as root,
it acts for SUDO_USER; it refuses to run serve as root.

install writes /etc/systemd/system/devingress.service, turns on lingering for you (so
/run/user/<uid>, which holds the admin socket, exists at boot), reloads systemd and starts the
unit. The unit runs this binary's path with symlinks resolved, after Docker when Docker is
installed, with XDG_RUNTIME_DIR=/run/user/<uid> plus XDG_CONFIG_HOME, XDG_STATE_HOME and
XDG_DATA_HOME when they are set here, so serve and your shell agree on config, state and the
admin socket. When ~/.config/systemd/user/devingress.service exists, install prints the command
that stops that user unit, for you to run first; it does not run it. Two serves cannot share the
admin socket, and the system unit retries every 5s until it is free.

uninstall stops and disables the unit, removes the file and reloads systemd. Lingering is left as
it is.

On macOS, serve binds loopback :80/:443 without root, so service needs no sudo, and refuses to
run as root. install writes ~/Library/LaunchAgents/com.github.wonderpanda.devingress.plist and
loads it with "launchctl bootstrap gui/<uid>", which starts serve now and at every login, and
restarts it when it fails. The agent runs this binary's path with symlinks resolved, with PATH,
XDG_CONFIG_HOME, XDG_STATE_HOME, XDG_DATA_HOME and XDG_RUNTIME_DIR when they are set here, and
logs to ~/Library/Logs/devingress.log. When the agent is already loaded, install boots it out
first. uninstall boots it out and removes the plist.

Each command is printed before it runs. After reinstalling the binary at the same path,
"sudo systemctl restart devingress" (Linux) or
"launchctl kickstart -k gui/$(id -u)/com.github.wonderpanda.devingress" (macOS) runs the new one.

Flags:
  --print    print the unit or plist and the commands as a shell script; run nothing, change nothing

Exit codes:
  0  installed, uninstalled, or printed
  1  not Linux with systemd or macOS with launchctl, serve would run as root, or a command failed
  2  bad flag or argument

Examples:
  devingress service install --print
  devingress service install
  devingress service uninstall
`

// What service touches on this machine; tests replace them.
var (
	serviceSupported = service.Supported
	serviceAccount   = func() (service.Account, error) {
		return service.Target(os.Geteuid() == 0, os.Getenv)
	}
	serviceExecutable = os.Executable
	serviceRun        = service.Exec
	launchdSupported  = func() error { return launchd.Supported(os.Geteuid(), exec.LookPath) }
	launchdLoaded     = launchd.Loaded
)

// hostOS picks what service and dns set up: systemd and systemd-resolved, or launchd and
// /etc/resolver on darwin. Tests replace it.
var hostOS = runtime.GOOS

func serviceCmd(args []string, stdout io.Writer) error {
	fs := flags("service")
	printOnly := fs.Bool("print", false, "print the unit and commands instead of running them")
	pos, err := positional(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || pos[0] != "install" && pos[0] != "uninstall" {
		return usageError{fmt.Errorf("expected install or uninstall, got %q", pos)}
	}
	if hostOS == "darwin" {
		return launchAgentCmd(pos[0], *printOnly, stdout)
	}
	if err := serviceSupported(); err != nil {
		return err
	}
	if pos[0] == "uninstall" {
		if err := runSteps(service.Uninstall(), *printOnly, serviceRun, stdout); err != nil {
			return err
		}
		note(stdout, *printOnly, `Lingering is left as it is; "sudo loginctl disable-linger <user>" turns it off.`)
		return nil
	}

	acct, err := serviceAccount()
	if err != nil {
		return err
	}
	exe, err := serviceExe()
	if err != nil {
		return err
	}
	if _, err := os.Stat(service.UserUnit(acct)); err == nil {
		const stop = "systemctl --user disable --now devingress"
		if *printOnly {
			note(stdout, true, fmt.Sprintf("A user unit exists at %s; stop it first, as %s.", service.UserUnit(acct), acct.Name))
			fmt.Fprintln(stdout, stop)
		} else {
			fmt.Fprintf(stdout, "A user unit exists at %s. Stop it yourself, as %s; until you do, the system\n"+
				"unit retries every 5s, since two serves cannot share the admin socket:\n  %s\n",
				service.UserUnit(acct), acct.Name, stop)
		}
	}
	return runSteps(service.Install(acct, service.Unit(acct, exe, os.Getenv)), *printOnly, serviceRun, stdout)
}

// launchAgentCmd installs or uninstalls the LaunchAgent: service on darwin.
func launchAgentCmd(sub string, printOnly bool, stdout io.Writer) error {
	if err := launchdSupported(); err != nil {
		return err
	}
	acct, err := serviceAccount()
	if err != nil {
		return err
	}
	loaded := launchdLoaded(acct.UID)
	if sub == "uninstall" {
		return runSteps(launchd.Uninstall(acct.UID, acct.Home, loaded), printOnly, serviceRun, stdout)
	}
	exe, err := serviceExe()
	if err != nil {
		return err
	}
	plist := launchd.Plist(acct.Home, exe, os.Getenv)
	return runSteps(launchd.Install(acct.UID, acct.Home, plist, loaded), printOnly, serviceRun, stdout)
}

// serviceExe is this binary's path with symlinks resolved, for the service to run.
func serviceExe() (string, error) {
	exe, err := serviceExecutable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// runSteps prints each step and runs it with run or, with printOnly, prints the steps as a shell
// script and runs nothing.
func runSteps(steps []service.Step, printOnly bool, run func(service.Step, io.Writer) error, stdout io.Writer) error {
	for _, s := range steps {
		switch {
		case printOnly && s.Stdin != "":
			fmt.Fprintf(stdout, "%s >/dev/null <<'EOF'\n%sEOF\n", s, s.Stdin)
		case printOnly:
			fmt.Fprintln(stdout, s)
		default:
			fmt.Fprintf(stdout, "+ %s\n", s)
			if err := run(s, stdout); err != nil {
				return err
			}
		}
	}
	return nil
}

// note prints lines of explanation, as comments when the output is a script.
func note(stdout io.Writer, printOnly bool, text string) {
	if printOnly {
		text = "# " + strings.ReplaceAll(text, "\n", "\n# ")
	}
	fmt.Fprintln(stdout, text)
}
