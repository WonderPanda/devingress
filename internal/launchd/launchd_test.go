package launchd

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/WonderPanda/devingress/internal/service"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestPlist(t *testing.T) {
	got := Plist("/Users/dev", "/opt/dev & co/devingress", env(map[string]string{
		"PATH":            "/opt/homebrew/bin:/usr/bin:/bin",
		"XDG_STATE_HOME":  "/Users/dev/<state>",
		"XDG_RUNTIME_DIR": "/Users/dev/run",
		"HOME":            "/elsewhere",
	}))
	want := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.github.wonderpanda.devingress</string>
	<key>ProgramArguments</key>
	<array>
		<string>/opt/dev &amp; co/devingress</string>
		<string>serve</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>/opt/homebrew/bin:/usr/bin:/bin</string>
		<key>XDG_STATE_HOME</key>
		<string>/Users/dev/&lt;state&gt;</string>
		<key>XDG_RUNTIME_DIR</key>
		<string>/Users/dev/run</string>
	</dict>
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
	<string>/Users/dev/Library/Logs/devingress.log</string>
	<key>StandardErrorPath</key>
	<string>/Users/dev/Library/Logs/devingress.log</string>
</dict>
</plist>
`
	if got != want {
		t.Errorf("plist:\n%s\nwant:\n%s", got, want)
	}

	args, err := ProgramArguments([]byte(got))
	if err != nil || !slices.Equal(args, []string{"/opt/dev & co/devingress", "serve"}) {
		t.Errorf("ProgramArguments: %q, %v", args, err)
	}
	if empty := Plist("/Users/dev", "/x", env(nil)); !strings.Contains(empty, "<key>EnvironmentVariables</key>\n\t<dict>\n\t</dict>\n") {
		t.Errorf("with nothing set, plist:\n%s", empty)
	}
}

func TestProgramArgumentsMissing(t *testing.T) {
	args, err := ProgramArguments([]byte(`<plist version="1.0"><dict><key>Label</key><string>x</string>` +
		`<key>EnvironmentVariables</key><dict><key>A</key><string>b</string></dict></dict></plist>`))
	if err != nil || args != nil {
		t.Errorf("no ProgramArguments: %q, %v", args, err)
	}
	if _, err := ProgramArguments([]byte("<plist><dict><key>")); err == nil {
		t.Error("truncated plist parsed")
	}
}

func lines(steps []service.Step) []string {
	var out []string
	for _, s := range steps {
		out = append(out, s.String())
	}
	return out
}

func TestSteps(t *testing.T) {
	install := []string{
		"mkdir -p /Users/dev/Library/LaunchAgents /Users/dev/Library/Logs",
		"tee /Users/dev/Library/LaunchAgents/com.github.wonderpanda.devingress.plist",
		"launchctl bootstrap gui/501 /Users/dev/Library/LaunchAgents/com.github.wonderpanda.devingress.plist",
	}
	steps := Install("501", "/Users/dev", "PLIST", false)
	if got := lines(steps); !slices.Equal(got, install) {
		t.Errorf("install: %q, want %q", got, install)
	}
	if steps[1].Stdin != "PLIST" || steps[0].Stdin != "" || steps[2].Stdin != "" {
		t.Errorf("stdin: %+v", steps)
	}
	bootout := "launchctl bootout gui/501/com.github.wonderpanda.devingress"
	if got := lines(Install("501", "/Users/dev", "PLIST", true)); !slices.Equal(got, append([]string{bootout}, install...)) {
		t.Errorf("install over a loaded agent: %q", got)
	}

	rm := "rm -f /Users/dev/Library/LaunchAgents/com.github.wonderpanda.devingress.plist"
	if got := lines(Uninstall("501", "/Users/dev", true)); !slices.Equal(got, []string{bootout, rm}) {
		t.Errorf("uninstall: %q", got)
	}
	if got := lines(Uninstall("501", "/Users/dev", false)); !slices.Equal(got, []string{rm}) {
		t.Errorf("uninstall, not loaded: %q", got)
	}
}

func TestSupported(t *testing.T) {
	found := func(string) (string, error) { return "/bin/launchctl", nil }
	missing := func(string) (string, error) {
		return "", errors.New(`exec: "launchctl": executable file not found in $PATH`)
	}
	if err := Supported(501, found); err != nil {
		t.Errorf("user with launchctl: %v", err)
	}
	if err := Supported(0, found); err == nil || !strings.Contains(err.Error(), "as your own user") {
		t.Errorf("root: %v", err)
	}
	if err := Supported(501, missing); err == nil || !strings.Contains(err.Error(), "needs launchctl") {
		t.Errorf("no launchctl: %v", err)
	}
}
