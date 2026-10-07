package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/WonderPanda/devingress/internal/service"
)

// fakeService replaces everything service touches with a dev account whose home is a temp dir,
// an executable reached through a symlink, and a runner that records each step instead of
// running it. It returns the home, the executable's real path, and the recorded steps.
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
	supported, account, executable, run := serviceSupported, serviceAccount, serviceExecutable, serviceRun
	t.Cleanup(func() {
		serviceSupported, serviceAccount, serviceExecutable, serviceRun = supported, account, executable, run
	})
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
