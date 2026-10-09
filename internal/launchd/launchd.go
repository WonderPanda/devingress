// Package launchd renders the macOS LaunchAgent that runs devingress serve at login, and the
// launchctl commands that install and remove it. None of them needs root.
package launchd

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/WonderPanda/devingress/internal/service"
)

// Label names the agent to launchd.
const Label = "com.github.wonderpanda.devingress"

// PlistPath is where install writes the agent for the user whose home is home.
func PlistPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist")
}

// LogPath is where the agent's stdout and stderr go.
func LogPath(home string) string { return filepath.Join(home, "Library", "Logs", "devingress.log") }

// envVars are passed to serve when set where install runs. launchd's default PATH lacks
// /opt/homebrew/bin, where mkcert usually is, and serve and the user's shell must agree on config,
// state, certificates and the admin socket.
var envVars = []string{"PATH", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR"}

// Plist renders the agent that runs exe's serve at login, restarting it when it fails.
func Plist(home, exe string, getenv func(string) string) string {
	var env strings.Builder
	for _, v := range envVars {
		if val := getenv(v); val != "" {
			fmt.Fprintf(&env, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n", v, escape(val))
		}
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>serve</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
%s	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>5</integer>
	<key>StandardOutPath</key>
	<string>%[4]s</string>
	<key>StandardErrorPath</key>
	<string>%[4]s</string>
</dict>
</plist>
`, Label, escape(exe), env.String(), escape(LogPath(home)))
}

func escape(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// ProgramArguments reads the ProgramArguments array from a plist, or nil when it has none.
func ProgramArguments(plist []byte) ([]string, error) {
	d := xml.NewDecoder(bytes.NewReader(plist))
	var key string
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch {
		case start.Name.Local == "key":
			if err := d.DecodeElement(&key, &start); err != nil {
				return nil, err
			}
			continue
		case start.Name.Local == "array" && key == "ProgramArguments":
			var arr struct {
				Strings []string `xml:"string"`
			}
			if err := d.DecodeElement(&arr, &start); err != nil {
				return nil, err
			}
			return arr.Strings, nil
		}
		key = ""
	}
}

// Domain is the launchctl domain of uid's GUI session, where the agent runs.
func Domain(uid string) string { return "gui/" + uid }

// Target is the agent in uid's GUI session.
func Target(uid string) string { return Domain(uid) + "/" + Label }

// Install writes plist to path and loads it, which starts serve (RunAtLoad). When the agent is
// already loaded it is booted out first: bootstrap refuses a loaded label.
func Install(uid, home, plist string, loaded bool) []service.Step {
	var steps []service.Step
	if loaded {
		steps = append(steps, service.Step{Args: []string{"launchctl", "bootout", Target(uid)}})
	}
	path := PlistPath(home)
	return append(steps,
		service.Step{Args: []string{"mkdir", "-p", filepath.Dir(path), filepath.Dir(LogPath(home))}},
		service.Step{Args: []string{"tee", path}, Stdin: plist},
		service.Step{Args: []string{"launchctl", "bootstrap", Domain(uid), path}},
	)
}

// Uninstall stops the agent, when it is loaded, and removes its plist.
func Uninstall(uid, home string, loaded bool) []service.Step {
	var steps []service.Step
	if loaded {
		steps = append(steps, service.Step{Args: []string{"launchctl", "bootout", Target(uid)}})
	}
	return append(steps, service.Step{Args: []string{"rm", "-f", PlistPath(home)}})
}

// Supported reports why this machine cannot run the agent as euid, if it cannot.
func Supported(euid int, lookPath func(string) (string, error)) error {
	if euid == 0 {
		return errors.New(`a LaunchAgent runs as the logged-in user who installs it: run "devingress service" ` +
			"as your own user, without sudo")
	}
	if _, err := lookPath("launchctl"); err != nil {
		return fmt.Errorf("devingress service needs launchctl: %w", err)
	}
	return nil
}

// Loaded reports whether the agent is loaded in uid's GUI session, with the read-only
// "launchctl print".
func Loaded(uid string) bool { return exec.Command("launchctl", "print", Target(uid)).Run() == nil }
