package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/WonderPanda/devingress/internal/launchd"
	"github.com/WonderPanda/devingress/internal/service"
)

// fakeService replaces everything service touches, on Linux, with a dev account whose home is a
// temp dir, an executable reached through a symlink, and a runner that records each step instead
// of running it. It returns the home, the executable's real path, and the recorded steps.
func fakeService(t *testing.T) (home, exe string, ran *[]service.Step) {
	t.Helper()
	isolate(t)
	t.Setenv("XDG_STATE_HOME", "/home/dev/state")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home, exe = filepath.Join(root, "home"), filepath.Join(root, "bin", "devingress")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "devingress-link")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	ran = &[]service.Step{}
	goos, supported, account, executable, run := hostOS, serviceSupported, serviceAccount, serviceExecutable, serviceRun
	t.Cleanup(func() {
		hostOS, serviceSupported, serviceAccount, serviceExecutable, serviceRun = goos, supported, account, executable, run
	})
	hostOS = "linux"
	serviceSupported = func() error { return nil }
	serviceAccount = func() (service.Account, error) {
		return service.Account{Name: "dev", Group: "devs", UID: "1000", GID: "1001", Home: home}, nil
	}
	serviceExecutable = func() (string, error) { return link, nil }
	serviceRun = func(s service.Step, _ io.Writer) error {
		*ran = append(*ran, s)
		return nil
	}
	return home, exe, ran
}

func userUnit(t *testing.T, home string) string {
	t.Helper()
	path := filepath.Join(home, ".config", "systemd", "user", "devingress.service")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func wantUnit(exe string) string {
	return `[Unit]
Description=devingress local HTTPS ingress
Requires=user-runtime-dir@1000.service
After=user-runtime-dir@1000.service network-online.target docker.service
Wants=network-online.target

[Service]
User=dev
Group=devs
Environment=XDG_RUNTIME_DIR=/run/user/1000
Environment=XDG_STATE_HOME=/home/dev/state
ExecStart=` + exe + ` serve
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
`
}

func stepLines(steps []service.Step) []string {
	var lines []string
	for _, s := range steps {
		lines = append(lines, s.String())
	}
	return lines
}

var (
	installCommands = []string{
		"sudo tee /etc/systemd/system/devingress.service",
		"sudo loginctl enable-linger dev",
		"sudo systemctl daemon-reload",
		"sudo systemctl enable --now devingress",
	}
	uninstallCommands = []string{
		"sudo systemctl disable --now devingress",
		"sudo rm -f /etc/systemd/system/devingress.service",
		"sudo systemctl daemon-reload",
	}
)

func TestServiceInstallPrint(t *testing.T) {
	home, exe, ran := fakeService(t)
	want := "sudo tee /etc/systemd/system/devingress.service >/dev/null <<'EOF'\n" + wantUnit(exe) + "EOF\n" +
		"sudo loginctl enable-linger dev\n" +
		"sudo systemctl daemon-reload\n" +
		"sudo systemctl enable --now devingress\n"
	for _, args := range [][]string{{"service", "install", "--print"}, {"service", "--print", "install"}} {
		code, stdout, stderr := runCLI(t, args...)
		if code != 0 || stderr != "" || stdout != want {
			t.Errorf("%v: exit %d, stderr %q, stdout:\n%s\nwant:\n%s", args, code, stderr, stdout, want)
		}
	}

	unit := userUnit(t, home)
	want = "# A user unit exists at " + unit + "; stop it first, as dev.\n" +
		"systemctl --user disable --now devingress\n" + want
	code, stdout, _ := runCLI(t, "service", "install", "--print")
	if code != 0 || stdout != want {
		t.Errorf("with a user unit: exit %d, stdout:\n%s\nwant:\n%s", code, stdout, want)
	}
	if len(*ran) != 0 {
		t.Errorf("--print ran %q", stepLines(*ran))
	}
}

func TestServiceInstall(t *testing.T) {
	home, exe, ran := fakeService(t)
	code, stdout, stderr := runCLI(t, "service", "install")
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if got := stepLines(*ran); !slices.Equal(got, installCommands) {
		t.Errorf("ran %q, want %q", got, installCommands)
	}
	if (*ran)[0].Stdin != wantUnit(exe) {
		t.Errorf("tee got:\n%s\nwant:\n%s", (*ran)[0].Stdin, wantUnit(exe))
	}
	if want := "+ " + strings.Join(installCommands, "\n+ ") + "\n"; stdout != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", stdout, want)
	}

	*ran = nil
	unit := userUnit(t, home)
	code, stdout, _ = runCLI(t, "service", "install")
	if code != 0 || !slices.Equal(stepLines(*ran), installCommands) {
		t.Errorf("with a user unit: exit %d, ran %q", code, stepLines(*ran))
	}
	warning, rest, _ := strings.Cut(stdout, "+ ")
	if !strings.HasPrefix(warning, "A user unit exists at "+unit+".") ||
		!strings.HasSuffix(warning, "\n  systemctl --user disable --now devingress\n") ||
		!strings.HasPrefix(rest, installCommands[0]) {
		t.Errorf("with a user unit, stdout:\n%s", stdout)
	}
}

func TestServiceUninstall(t *testing.T) {
	_, _, ran := fakeService(t)
	const linger = `Lingering is left as it is; "sudo loginctl disable-linger <user>" turns it off.` + "\n"
	code, stdout, stderr := runCLI(t, "service", "uninstall", "--print")
	if want := strings.Join(uninstallCommands, "\n") + "\n# " + linger; code != 0 || stderr != "" || stdout != want {
		t.Errorf("--print: exit %d, stderr %q, stdout:\n%s\nwant:\n%s", code, stderr, stdout, want)
	}
	if len(*ran) != 0 {
		t.Errorf("--print ran %q", stepLines(*ran))
	}

	code, stdout, stderr = runCLI(t, "service", "uninstall")
	if code != 0 || stderr != "" || !slices.Equal(stepLines(*ran), uninstallCommands) {
		t.Errorf("exit %d, stderr %q, ran %q", code, stderr, stepLines(*ran))
	}
	if want := "+ " + strings.Join(uninstallCommands, "\n+ ") + "\n" + linger; stdout != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", stdout, want)
	}
}

func TestServiceFailures(t *testing.T) {
	_, _, ran := fakeService(t)
	serviceRun = func(s service.Step, _ io.Writer) error {
		*ran = append(*ran, s)
		if s.Args[1] == "loginctl" {
			return errors.New("sudo loginctl enable-linger dev: exit status 1")
		}
		return nil
	}
	code, _, stderr := runCLI(t, "service", "install")
	if code != 1 || len(*ran) != 2 || !strings.Contains(stderr, "loginctl") {
		t.Errorf("failing step: exit %d, ran %q, stderr %q", code, stepLines(*ran), stderr)
	}

	serviceAccount = func() (service.Account, error) { return service.Account{}, errors.New("would run serve as root") }
	if code, stdout, stderr := runCLI(t, "service", "install", "--print"); code != 1 || stdout != "" ||
		!strings.Contains(stderr, "as root") {
		t.Errorf("root: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	serviceSupported = func() error { return errors.New("devingress service needs Linux with systemd, not darwin") }
	for _, sub := range []string{"install", "uninstall"} {
		code, stdout, stderr := runCLI(t, "service", sub, "--print")
		if code != 1 || stdout != "" || stderr != "devingress: devingress service needs Linux with systemd, not darwin\n" {
			t.Errorf("%s without systemd: exit %d, stdout %q, stderr %q", sub, code, stdout, stderr)
		}
	}

	for _, args := range [][]string{{"service"}, {"service", "start"}, {"service", "install", "uninstall"}} {
		if code, _, stderr := runCLI(t, args...); code != 2 || !strings.Contains(stderr, "expected install or uninstall") {
			t.Errorf("%v: exit %d, stderr %q", args, code, stderr)
		}
	}
}

// fakeLaunchAgent is fakeService on macOS, as a user with launchctl, with the agent loaded or not.
func fakeLaunchAgent(t *testing.T, loaded bool) (home, exe string, ran *[]service.Step) {
	t.Helper()
	home, exe, ran = fakeService(t)
	t.Setenv("PATH", "/opt/homebrew/bin:/usr/bin:/bin")
	goos, supported, isLoaded := hostOS, launchdSupported, launchdLoaded
	t.Cleanup(func() { hostOS, launchdSupported, launchdLoaded = goos, supported, isLoaded })
	hostOS = "darwin"
	launchdSupported = func() error {
		return launchd.Supported(1000, func(string) (string, error) { return "/bin/launchctl", nil })
	}
	launchdLoaded = func(uid string) bool {
		if uid != "1000" {
			t.Errorf("probed uid %s, want 1000", uid)
		}
		return loaded
	}
	return home, exe, ran
}

func TestLaunchAgentInstallPrint(t *testing.T) {
	for _, loaded := range []bool{false, true} {
		home, exe, ran := fakeLaunchAgent(t, loaded)
		userUnit(t, home) // Linux's user unit means nothing on macOS
		plistPath := home + "/Library/LaunchAgents/com.github.wonderpanda.devingress.plist"
		plist := launchd.Plist(home, exe, os.Getenv)
		want := "mkdir -p " + home + "/Library/LaunchAgents " + home + "/Library/Logs\n" +
			"tee " + plistPath + " >/dev/null <<'EOF'\n" + plist + "EOF\n" +
			"launchctl bootstrap gui/1000 " + plistPath + "\n"
		if loaded {
			want = "launchctl bootout gui/1000/com.github.wonderpanda.devingress\n" + want
		}
		code, stdout, stderr := runCLI(t, "service", "install", "--print")
		if code != 0 || stderr != "" || stdout != want {
			t.Errorf("loaded=%v: exit %d, stderr %q, stdout:\n%s\nwant:\n%s", loaded, code, stderr, stdout, want)
		}
		for _, s := range []string{"<string>" + exe + "</string>", "<key>PATH</key>\n\t\t<string>/opt/homebrew/bin:/usr/bin:/bin</string>",
			"<key>XDG_STATE_HOME</key>\n\t\t<string>/home/dev/state</string>", "<key>XDG_RUNTIME_DIR</key>"} {
			if !strings.Contains(plist, s) {
				t.Errorf("plist lacks %q:\n%s", s, plist)
			}
		}
		if strings.Contains(plist, "XDG_CONFIG_HOME") {
			t.Errorf("plist sets the unset XDG_CONFIG_HOME:\n%s", plist)
		}
		if len(*ran) != 0 {
			t.Errorf("--print ran %q", stepLines(*ran))
		}
	}
}

func TestLaunchAgentInstall(t *testing.T) {
	home, exe, ran := fakeLaunchAgent(t, false)
	code, stdout, stderr := runCLI(t, "service", "install")
	want := []string{
		"mkdir -p " + home + "/Library/LaunchAgents " + home + "/Library/Logs",
		"tee " + home + "/Library/LaunchAgents/com.github.wonderpanda.devingress.plist",
		"launchctl bootstrap gui/1000 " + home + "/Library/LaunchAgents/com.github.wonderpanda.devingress.plist",
	}
	if code != 0 || stderr != "" || !slices.Equal(stepLines(*ran), want) {
		t.Fatalf("exit %d, stderr %q, ran %q, want %q", code, stderr, stepLines(*ran), want)
	}
	if (*ran)[1].Stdin != launchd.Plist(home, exe, os.Getenv) {
		t.Errorf("tee got:\n%s", (*ran)[1].Stdin)
	}
	if wantOut := "+ " + strings.Join(want, "\n+ ") + "\n"; stdout != wantOut {
		t.Errorf("stdout:\n%s\nwant:\n%s", stdout, wantOut)
	}
}

func TestLaunchAgentUninstall(t *testing.T) {
	for _, loaded := range []bool{false, true} {
		home, _, ran := fakeLaunchAgent(t, loaded)
		want := []string{"rm -f " + home + "/Library/LaunchAgents/com.github.wonderpanda.devingress.plist"}
		if loaded {
			want = append([]string{"launchctl bootout gui/1000/com.github.wonderpanda.devingress"}, want...)
		}
		code, stdout, stderr := runCLI(t, "service", "uninstall", "--print")
		if code != 0 || stderr != "" || stdout != strings.Join(want, "\n")+"\n" || len(*ran) != 0 {
			t.Errorf("loaded=%v --print: exit %d, stderr %q, ran %q, stdout:\n%s", loaded, code, stderr, stepLines(*ran), stdout)
		}
		code, stdout, stderr = runCLI(t, "service", "uninstall")
		if code != 0 || stderr != "" || !slices.Equal(stepLines(*ran), want) || stdout != "+ "+strings.Join(want, "\n+ ")+"\n" {
			t.Errorf("loaded=%v: exit %d, stderr %q, ran %q, stdout:\n%s", loaded, code, stderr, stepLines(*ran), stdout)
		}
	}
}

func TestLaunchAgentRefusesRoot(t *testing.T) {
	_, _, ran := fakeLaunchAgent(t, true)
	launchdSupported = func() error {
		return launchd.Supported(0, func(string) (string, error) { return "/bin/launchctl", nil })
	}
	for _, sub := range []string{"install", "uninstall"} {
		for _, args := range [][]string{{"service", sub, "--print"}, {"service", sub}} {
			code, stdout, stderr := runCLI(t, args...)
			if code != 1 || stdout != "" || !strings.Contains(stderr, "as your own user, without sudo") {
				t.Errorf("%v as root: exit %d, stdout %q, stderr %q", args, code, stdout, stderr)
			}
		}
	}
	if len(*ran) != 0 {
		t.Errorf("as root, ran %q", stepLines(*ran))
	}
}
