package service

import (
	"os/user"
	"strings"
	"testing"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

var dev = Account{Name: "dev", Group: "devs", UID: "1000", GID: "1001", Home: "/home/dev"}

func TestUnit(t *testing.T) {
	got := Unit(dev, "/opt/devingress/bin/devingress", env(nil))
	want := `[Unit]
Description=devingress local HTTPS ingress
Requires=user-runtime-dir@1000.service
After=user-runtime-dir@1000.service network-online.target docker.service
Wants=network-online.target

[Service]
User=dev
Group=devs
Environment=XDG_RUNTIME_DIR=/run/user/1000
ExecStart=/opt/devingress/bin/devingress serve
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
`
	if got != want {
		t.Errorf("unit:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnitXDG(t *testing.T) {
	got := Unit(dev, "/x/devingress", env(map[string]string{
		"XDG_STATE_HOME":  "/s",
		"XDG_DATA_HOME":   "/d",
		"XDG_RUNTIME_DIR": "/tmp/not-this-one",
		"HOME":            "/elsewhere",
	}))
	want := "Group=devs\n" +
		"Environment=XDG_RUNTIME_DIR=/run/user/1000\n" +
		"Environment=XDG_STATE_HOME=/s\n" +
		"Environment=XDG_DATA_HOME=/d\n" +
		"ExecStart=/x/devingress serve\n"
	if !strings.Contains(got, want) {
		t.Errorf("unit:\n%s\nwant it to contain:\n%s", got, want)
	}
	if strings.Count(got, "Environment=") != 3 {
		t.Errorf("unit sets unset or extra variables:\n%s", got)
	}
}

func TestTarget(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if me.Uid == "0" {
		t.Skip("run as root: there is no non-root caller to target")
	}
	want := Account{Name: me.Username, UID: me.Uid, GID: me.Gid, Home: me.HomeDir, Group: me.Gid}
	if g, err := user.LookupGroupId(me.Gid); err == nil {
		want.Group = g.Name
	}

	// SUDO_USER means nothing unless the caller is root.
	got, err := Target(false, env(map[string]string{"SUDO_USER": "root"}))
	if err != nil || got != want {
		t.Errorf("as %s: %+v, %v; want %+v", me.Username, got, err, want)
	}
	got, err = Target(true, env(map[string]string{"SUDO_USER": me.Username, "SUDO_UID": me.Uid}))
	if err != nil || got != want {
		t.Errorf("as root, SUDO_USER=%s: %+v, %v; want %+v", me.Username, got, err, want)
	}
	for _, vars := range []map[string]string{{"SUDO_USER": "root", "SUDO_UID": "0"}, nil} {
		if got, err := Target(true, env(vars)); err == nil || !strings.Contains(err.Error(), "as root") {
			t.Errorf("as root, %v: %+v, %v; want a refusal", vars, got, err)
		}
	}
}

func TestSteps(t *testing.T) {
	steps := Install(dev, "UNIT")
	var got []string
	for _, s := range steps {
		got = append(got, s.String())
	}
	want := []string{
		"sudo tee /etc/systemd/system/devingress.service",
		"sudo loginctl enable-linger dev",
		"sudo systemctl daemon-reload",
		"sudo systemctl enable --now devingress",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("install: %q, want %q", got, want)
	}
	if steps[0].Stdin != "UNIT" {
		t.Errorf("tee stdin = %q, want the unit", steps[0].Stdin)
	}
	for _, s := range steps[1:] {
		if s.Stdin != "" {
			t.Errorf("%s has stdin %q", s, s.Stdin)
		}
	}
}
