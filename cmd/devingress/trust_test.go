package main

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smallstep/truststore"
)

func TestMain(m *testing.M) {
	// The real installer changes this machine's trust stores: no test may reach it.
	installRoot = func(*x509.Certificate) error { panic("real installRoot called from a test") }
	uninstallRoot = func(*x509.Certificate) error { panic("real uninstallRoot called from a test") }
	os.Exit(m.Run())
}

// fakeStores replaces the installer and records the certificates it is given.
func fakeStores(t *testing.T) (installed, uninstalled *[]*x509.Certificate) {
	t.Helper()
	installed, uninstalled = new([]*x509.Certificate), new([]*x509.Certificate)
	origInstall, origUninstall := installRoot, uninstallRoot
	installRoot = func(c *x509.Certificate) error { *installed = append(*installed, c); return nil }
	uninstallRoot = func(c *x509.Certificate) error { *uninstalled = append(*uninstalled, c); return nil }
	t.Cleanup(func() { installRoot, uninstallRoot = origInstall, origUninstall })
	return installed, uninstalled
}

func TestTrustMkcertRoot(t *testing.T) {
	isolate(t)
	caroot := t.TempDir()
	for _, f := range []string{"rootCA.pem", "rootCA-key.pem"} {
		if err := os.WriteFile(filepath.Join(caroot, f), []byte("not read"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CAROOT", caroot)
	installed, uninstalled := fakeStores(t)
	root := filepath.Join(caroot, "rootCA.pem")

	code, stdout, stderr := runCLI(t, "trust")
	want := "using mkcert's root at " + root + `; run "mkcert -install" to trust it` + "\n"
	if code != 0 || stdout != want || stderr != "" {
		t.Errorf("trust: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	code, stdout, _ = runCLI(t, "trust", "--uninstall")
	if code != 0 || !strings.HasPrefix(stdout, "using mkcert's root at "+root+";") {
		t.Errorf("trust --uninstall: exit %d, stdout %q", code, stdout)
	}
	code, stdout, _ = runCLI(t, "trust", "--path")
	if code != 0 || stdout != root+"\n" {
		t.Errorf("trust --path: exit %d, stdout %q, want %q", code, stdout, root)
	}
	if len(*installed)+len(*uninstalled) != 0 {
		t.Errorf("the installer was called with mkcert's root in use")
	}
}

func TestTrustCaddyRoot(t *testing.T) {
	isolate(t)
	t.Setenv("CAROOT", t.TempDir()) // no rootCA-key.pem: no mkcert root
	installed, uninstalled := fakeStores(t)
	root := filepath.Join(os.Getenv("XDG_DATA_HOME"), "devingress", "caddy", "pki", "authorities", "local", "root.crt")

	code, stdout, _ := runCLI(t, "trust", "--path")
	if code != 0 || stdout != root+"\n" {
		t.Errorf("trust --path: exit %d, stdout %q, want %q", code, stdout, root)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("--path created the root, or it existed already: %v", err)
	}

	code, stdout, stderr := runCLI(t, "trust")
	if code != 0 || stderr != "" {
		t.Fatalf("trust: exit %d, stderr %q", code, stderr)
	}
	t.Logf("devingress trust, with a fake installer:\n%s", stdout)
	cert, err := truststore.ReadCertificate(root)
	if err != nil {
		t.Fatalf("trust did not create the root: %v", err)
	}
	if !strings.Contains(stdout, "installing "+root+"\n  subject: "+cert.Subject.String()+"\ninto ") {
		t.Errorf("trust did not say what it installs:\n%s", stdout)
	}
	if len(*installed) != 1 || !(*installed)[0].Equal(cert) {
		t.Errorf("installed %d certificates, want exactly %s", len(*installed), root)
	}

	code, stdout, _ = runCLI(t, "trust", "--uninstall")
	if code != 0 || !strings.Contains(stdout, "removing "+root+"\n") {
		t.Errorf("trust --uninstall: exit %d, stdout %q", code, stdout)
	}
	if len(*uninstalled) != 1 || !(*uninstalled)[0].Equal(cert) {
		t.Errorf("uninstalled %d certificates, want exactly %s", len(*uninstalled), root)
	}
	if len(*installed) != 1 {
		t.Errorf("--uninstall installed")
	}

	code, stdout, _ = runCLI(t, "trust", "--path")
	if code != 0 || stdout != root+"\n" {
		t.Errorf("trust --path with the root created: exit %d, stdout %q", code, stdout)
	}
}

func TestTrustExclusiveFlags(t *testing.T) {
	isolate(t)
	code, stdout, stderr := runCLI(t, "trust", "--path", "--uninstall")
	if code != 2 || stdout != "" || !strings.Contains(stderr, "exclusive") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}
