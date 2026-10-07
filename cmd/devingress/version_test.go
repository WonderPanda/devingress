package main

import (
	"runtime/debug"
	"testing"
)

func TestAppVersion(t *testing.T) {
	saveVersion, saveReadBuildInfo := version, readBuildInfo
	t.Cleanup(func() { version, readBuildInfo = saveVersion, saveReadBuildInfo })

	version = "v1.2.3"
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		t.Fatal("build info read despite an ldflags version")
		return nil, false
	}
	if got := appVersion(); got != "v1.2.3" {
		t.Errorf("ldflags version: got %q, want v1.2.3", got)
	}

	version = ""
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Main: debug.Module{Version: "v0.4.5"}}, true
	}
	if got := appVersion(); got != "v0.4.5" {
		t.Errorf("module version: got %q, want v0.4.5", got)
	}

	for _, info := range []struct {
		name string
		info *debug.BuildInfo
		ok   bool
	}{
		{"no build info", nil, false},
		{"devel module", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, true},
		{"empty module version", &debug.BuildInfo{Main: debug.Module{Version: ""}}, true},
	} {
		readBuildInfo = func() (*debug.BuildInfo, bool) { return info.info, info.ok }
		if got := appVersion(); got != "dev" {
			t.Errorf("%s: got %q, want dev", info.name, got)
		}
	}
}

func TestVersionCommand(t *testing.T) {
	isolate(t)
	saveVersion := version
	t.Cleanup(func() { version = saveVersion })
	version = "v1.2.3"

	for _, args := range [][]string{{"version"}, {"--version"}} {
		code, stdout, stderr := runCLI(t, args...)
		if code != 0 || stdout != "v1.2.3\n" || stderr != "" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
	}

	code, _, stderr := runCLI(t, "version", "extra")
	if code != 2 || stderr == "" {
		t.Errorf("version extra: exit %d, stderr %q", code, stderr)
	}
}
