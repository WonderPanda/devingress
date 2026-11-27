package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeToken matches caddy-dns/cloudflare's format check; it is no real token.
const fakeToken = "cfut_" + "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN"

// isolateConfig points config.toml and the home directory at temp dirs and writes body as
// config.toml. It returns the config dir and the home dir.
func isolateConfig(t *testing.T, body string) (cfgDir, home string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("CAROOT", t.TempDir()) // no mkcert root: keeps Load from running mkcert
	cfgDir = filepath.Dir(ConfigFile())
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ConfigFile(), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgDir, home
}

func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func TestLoadWithoutRemote(t *testing.T) {
	isolateConfig(t, "http_port = 18080\n")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Remote != nil || c.Render().Remote != nil || c.Suffix() != "" || len(c.Warnings) != 0 {
		t.Errorf("remote mode on without [remote]: %+v", c)
	}
}

func TestLoadRemote(t *testing.T) {
	t.Run("tilde path", func(t *testing.T) {
		_, home := isolateConfig(t, "[remote]\nsuffix = \"Machine.Example.dev.\"\ndns_provider = \"cloudflare\"\ntoken_file = \"~/cf-token\"\n")
		writeFile(t, filepath.Join(home, "cf-token"), fakeToken+"\n", 0o600)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		want := Remote{Suffix: "machine.example.dev", DNSProvider: "cloudflare", TokenFile: filepath.Join(home, "cf-token")}
		if *c.Remote != want {
			t.Errorf("remote = %+v, want %+v", *c.Remote, want)
		}
		if r := c.Render().Remote; r == nil || r.Suffix != want.Suffix || r.TokenFile != want.TokenFile {
			t.Errorf("render options = %+v", r)
		}
		if len(c.Warnings) != 0 {
			t.Errorf("warnings for a mode-600 file: %q", c.Warnings)
		}
	})
	t.Run("relative path, default provider, CRLF", func(t *testing.T) {
		dir, _ := isolateConfig(t, "[remote]\nsuffix = \"machine.example.dev\"\ntoken_file = \"cf-token\"\n")
		writeFile(t, filepath.Join(dir, "cf-token"), fakeToken+"\r\n", 0o600)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.Remote.TokenFile != filepath.Join(dir, "cf-token") || c.Remote.DNSProvider != "cloudflare" {
			t.Errorf("remote = %+v", *c.Remote)
		}
	})
	t.Run("readable by others", func(t *testing.T) {
		_, home := isolateConfig(t, "[remote]\nsuffix = \"machine.example.dev\"\ntoken_file = \"~/cf-token\"\n")
		path := filepath.Join(home, "cf-token")
		writeFile(t, path, fakeToken, 0o600)
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "chmod 600 "+path) {
			t.Errorf("warnings = %q, want one saying chmod 600 %s", c.Warnings, path)
		}
	})
}

func TestLoadRemoteErrors(t *testing.T) {
	for name, c := range map[string]struct {
		config, token string // token "" writes no file
		want          string
	}{
		"missing file":     {`token_file = "~/cf-token"`, "", "does not exist"},
		"empty file":       {`token_file = "~/cf-token"`, "\n", "is empty"},
		"bad format":       {`token_file = "~/cf-token"`, "Bearer " + fakeToken + "\n", "does not hold a Cloudflare API token"},
		"quoted":           {`token_file = "~/cf-token"`, `"` + fakeToken + `"`, "does not hold a Cloudflare API token"},
		"two newlines":     {`token_file = "~/cf-token"`, fakeToken + "\n\n", "does not hold a Cloudflare API token"},
		"no token_file":    {``, "", "token_file is required"},
		"other provider":   {"dns_provider = \"route53\"\ntoken_file = \"~/cf-token\"", fakeToken, `dns_provider "route53" is not supported`},
		"brace in path":    {`token_file = "~/cf{token}"`, "", "cannot contain {"},
		"one-label suffix": {"token_file = \"~/cf-token\"\nsuffix = \"dev\"", fakeToken, "is not a DNS name of two or more labels"},
	} {
		t.Run(name, func(t *testing.T) {
			suffix := "suffix = \"machine.example.dev\"\n"
			if strings.Contains(c.config, "suffix") {
				suffix = ""
			}
			_, home := isolateConfig(t, "[remote]\n"+suffix+c.config+"\n")
			if c.token != "" {
				writeFile(t, filepath.Join(home, "cf-token"), c.token, 0o600)
			}
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
			if strings.Contains(err.Error(), fakeToken) {
				t.Fatalf("error quotes the token: %v", err)
			}
		})
	}
}

func TestRedact(t *testing.T) {
	_, home := isolateConfig(t, "[remote]\nsuffix = \"machine.example.dev\"\ntoken_file = \"~/cf-token\"\n")
	writeFile(t, filepath.Join(home, "cf-token"), fakeToken+"\n", 0o600)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	// The error caddy-dns/cloudflare's Provision returns for a token it rejects.
	caddyErr := errors.New("loading module 'cloudflare': API token '" + fakeToken + "' appears invalid")
	got := c.Redact(caddyErr).Error()
	if strings.Contains(got, fakeToken) || !strings.Contains(got, "API token '[redacted token]' appears invalid") {
		t.Errorf("Redact = %q", got)
	}
	if c.Redact(nil) != nil {
		t.Error("Redact(nil) != nil")
	}
	if other := errors.New("unrelated"); c.Redact(other) != other {
		t.Error("Redact changed an error without the token")
	}
}
