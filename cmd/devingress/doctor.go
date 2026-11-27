package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/WonderPanda/devingress/internal/doctor"
)

const doctorHelp = `Diagnose why a devingress URL does not load: serve, the bound ports, the bind addresses, the
DNS listener (when dns_listen is set), the root CA's trust, every registered owner, every
registered host's DNS and TLS and whether "devingress dns setup" routes it (a resolved drop-in,
or an /etc/resolver file on macOS), every registered upstream (warn when nothing listens), and
(when installed) the systemd unit or LaunchAgent. Prints one line per check: ok, warn or fail,
the subject, and for anything not ok a fix. Warnings don't affect the exit code.

In remote mode (a [remote] section in config.toml) it also checks: that Tailscale forwards the
tailnet's port 443 as raw TCP to 127.0.0.1:<https_port>; the tailnet address (one 100.64.0.0/10
address on tailscale0, or a utun interface on macOS); that a random name under the suffix
resolves to it; that serve, on loopback and then through the tailnet address, presents a
*.<suffix> certificate the system roots trust (warn within 14 days of expiry); that Cloudflare
reports the token active (warn within 30 days of its expiry; the token is never printed); and
each host of a remote = true route file that gets no remote URL.

Flags:
  --json    print a JSON array of {"check","subject","status","message","fix"} instead

Strictly read-only: never writes state or config, never loads config into a running serve, never
runs sudo, systemctl, launchctl or any other external command, except "tailscale serve status
--json" in remote mode (the CLI on PATH, else inside /Applications/Tailscale.app on macOS).

Exit codes:
  0  printed; no check failed (warnings are fine)
  1  at least one check failed
  2  bad flag or argument

Examples:
  devingress doctor
  devingress doctor --json
`

// errDoctorFailed tells run to exit 1 without printing anything more: doctor
// already printed every failing check.
var errDoctorFailed = errors.New("doctor: a check failed")

func doctorCmd(args []string, stdout io.Writer) error {
	fs := flags("doctor")
	asJSON := fs.Bool("json", false, "print a JSON array instead")
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Errorf("unexpected argument %q", fs.Arg(0))}
	}

	checks := doctor.Run(doctor.DefaultDeps())
	if *asJSON {
		out, err := json.MarshalIndent(checks, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s\n", out)
	} else {
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		for _, c := range checks {
			line := fmt.Sprintf("%s\t%s\t%s: %s", c.Status, c.Check, c.Subject, c.Message)
			if c.Status != doctor.OK {
				line += "\t(fix: " + c.Fix + ")"
			}
			fmt.Fprintln(tw, line)
		}
		tw.Flush()
	}

	if anyFailed(checks) {
		return errDoctorFailed
	}
	return nil
}

func anyFailed(checks []doctor.Check) bool {
	for _, c := range checks {
		if c.Status == doctor.Fail {
			return true
		}
	}
	return false
}
