package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/WonderPanda/devingress/internal/doctor"
)

func TestAnyFailed(t *testing.T) {
	if anyFailed([]doctor.Check{{Status: doctor.OK}, {Status: doctor.Warn}}) {
		t.Error("ok + warn: want exit 0")
	}
	if !anyFailed([]doctor.Check{{Status: doctor.OK}, {Status: doctor.Fail}, {Status: doctor.Warn}}) {
		t.Error("with a fail: want exit 1")
	}
}

type doctorCheck struct {
	Check   string `json:"check"`
	Subject string `json:"subject"`
	Status  string `json:"status"`
	Message string `json:"message"`
	Fix     string `json:"fix"`
}

func TestDoctorJSON(t *testing.T) {
	isolate(t)
	code, stdout, stderr := runCLI(t, "doctor", "--json")
	if stderr != "" {
		t.Fatalf("stderr: %q", stderr)
	}
	if code != 1 {
		t.Fatalf("exit %d, want 1 (serve is not running in an isolated env): %s", code, stdout)
	}
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.DisallowUnknownFields()
	var checks []doctorCheck
	if err := dec.Decode(&checks); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	if len(checks) == 0 {
		t.Fatal("no checks printed")
	}
	if checks[0].Check != "config" || checks[0].Status != "ok" {
		t.Errorf("config check: %+v", checks[0])
	}
	found := false
	for _, c := range checks {
		if c.Check == "serve" {
			found = true
			if c.Status != "fail" || c.Fix == "" {
				t.Errorf("serve check: %+v", c)
			}
		}
	}
	if !found {
		t.Errorf("no serve check in %+v", checks)
	}
}

func TestDoctorText(t *testing.T) {
	isolate(t)
	code, stdout, stderr := runCLI(t, "doctor")
	if stderr != "" {
		t.Fatalf("stderr: %q", stderr)
	}
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, stdout)
	}
	if !strings.Contains(stdout, "fail") || !strings.Contains(stdout, "fix:") {
		t.Errorf("stdout missing a fail line with its fix:\n%s", stdout)
	}
}
