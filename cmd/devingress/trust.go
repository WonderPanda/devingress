package main

import (
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/smallstep/truststore"

	"github.com/WonderPanda/devingress/internal/config"
	"github.com/WonderPanda/devingress/internal/ingress"
)

const trustHelp = `Install the root certificate devingress signs with into this machine's trust stores, so
browsers and tools trust its certificates. Run it once; mkcert is not needed.

The root is the one serve uses: rootCA.pem in ca_root, which defaults to mkcert's CAROOT. When that
is mkcert's root, trust changes nothing and tells you to run "mkcert -install" instead. Otherwise
it is the root Caddy mints under $XDG_DATA_HOME/devingress/caddy/pki/authorities/local; if that
does not exist yet, trust creates it by validating the registered routes as up does, without
touching a running serve.

Before installing, trust prints the root's path and subject and the stores it goes into:
  system   the OS store, through sudo (asks for your password)
  NSS      ~/.pki/nssdb (Chrome, Chromium) and Firefox profiles, when certutil is installed
  Java     $JAVA_HOME's cacerts, when JAVA_HOME is set

Node.js ignores the OS store. Point it at the root with NODE_EXTRA_CA_CERTS (see --path).

Flags:
  --uninstall   remove the root from the same stores instead
  --path        print only the root's absolute path, whether or not it exists yet; changes nothing

Exit codes:
  0  installed, removed, path printed, or mkcert's root is in use
  1  the root could not be created or read, or a store refused it
  2  bad flag or argument

Examples:
  devingress trust
  NODE_EXTRA_CA_CERTS=$(devingress trust --path) npm run dev
`

const trustStores = "the system store (via sudo), NSS (~/.pki/nssdb, Firefox) and Java (when JAVA_HOME is set)"

// installRoot and uninstallRoot change this machine's trust stores. Tests
// replace them and must never reach the real ones.
var (
	installRoot = func(cert *x509.Certificate) error {
		return truststore.Install(cert, truststore.WithFirefox(), truststore.WithJava())
	}
	uninstallRoot = func(cert *x509.Certificate) error {
		return truststore.Uninstall(cert, truststore.WithFirefox(), truststore.WithJava())
	}
)

func trust(args []string, stdout io.Writer) error {
	fs := flags("trust")
	uninstall := fs.Bool("uninstall", false, "remove the root from the trust stores")
	pathOnly := fs.Bool("path", false, "print only the root's path")
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Errorf("unexpected argument %q", fs.Arg(0))}
	}
	if *uninstall && *pathOnly {
		return usageError{errors.New("--uninstall and --path are exclusive")}
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	path, mkcert, err := cfg.RootCert()
	if err != nil {
		return err
	}
	switch {
	case *pathOnly:
		fmt.Fprintln(stdout, path)
		return nil
	case mkcert && *uninstall:
		fmt.Fprintf(stdout, "using mkcert's root at %s; run \"mkcert -uninstall\" to untrust it\n", path)
		return nil
	case mkcert:
		fmt.Fprintf(stdout, "using mkcert's root at %s; run \"mkcert -install\" to trust it\n", path)
		return nil
	}

	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if *uninstall {
			fmt.Fprintf(stdout, "no root at %s; nothing to uninstall\n", path)
			return nil
		}
		fmt.Fprintf(stdout, "creating devingress's root at %s\n", path)
		if err := ingress.Provision(); err != nil {
			return fmt.Errorf("creating the root: %w", err)
		}
	}
	cert, err := truststore.ReadCertificate(path)
	if err != nil {
		return err
	}
	if *uninstall {
		fmt.Fprintf(stdout, "removing %s\n  subject: %s\nfrom %s\n", path, cert.Subject, trustStores)
		return uninstallRoot(cert)
	}
	fmt.Fprintf(stdout, "installing %s\n  subject: %s\ninto %s\n", path, cert.Subject, trustStores)
	return installRoot(cert)
}
