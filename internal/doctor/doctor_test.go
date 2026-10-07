package doctor

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/WonderPanda/devingress/internal/routefile"
	"github.com/WonderPanda/devingress/internal/service"
)

func TestCheckConfig(t *testing.T) {
	if got := checkConfig("/x/config.toml", nil); got.Status != OK || got.Subject != "/x/config.toml" {
		t.Errorf("pass: %+v", got)
	}
	got := checkConfig("/x/config.toml", errors.New("line 3: bad key"))
	if got.Status != Fail || got.Fix != "line 3: bad key" {
		t.Errorf("fail: %+v", got)
	}
}

func acceptOnce(t *testing.T, ln net.Listener) {
	t.Helper()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		c.Close()
	}()
}

func TestCheckServe(t *testing.T) {
	deps := DefaultDeps()
	sock := filepath.Join(t.TempDir(), "admin.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	acceptOnce(t, ln)

	if got := checkServe(deps, sock); got.Status != OK {
		t.Errorf("pass: %+v", got)
	}
	if got := checkServe(deps, filepath.Join(t.TempDir(), "missing.sock")); got.Status != Fail {
		t.Errorf("fail: %+v", got)
	}
}

// freePort opens and immediately closes a TCP listener, for a port number
// nothing else is bound to at the moment of the call.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestCheckPorts(t *testing.T) {
	deps := DefaultDeps()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	acceptOnce(t, ln)
	openPort := ln.Addr().(*net.TCPAddr).Port
	closedPort := freePort(t)

	checks := checkPorts(deps, []string{"127.0.0.1"}, openPort, closedPort)
	if len(checks) != 2 {
		t.Fatalf("got %d checks, want 2: %+v", len(checks), checks)
	}
	if checks[0].Check != "ports" || checks[0].Status != OK {
		t.Errorf("open http_port: %+v", checks[0])
	}
	if checks[1].Status != Fail {
		t.Errorf("closed https_port: %+v", checks[1])
	}
}

func TestCheckBind(t *testing.T) {
	ifaces := []Iface{
		{Name: "lo", Addrs: []net.IP{net.ParseIP("127.0.0.1")}},
		{Name: "docker0", Addrs: []net.IP{net.ParseIP("172.17.0.1")}},
	}
	deps := Deps{Interfaces: func() ([]Iface, error) { return ifaces, nil }}

	checks := checkBind(deps, []string{"127.0.0.1"})
	if len(checks) != 2 || checks[0].Status != OK || checks[1].Subject != "docker0" || checks[1].Status != Warn {
		t.Errorf("bound address + unlisted docker0: %+v", checks)
	}

	checks = checkBind(deps, []string{"127.0.0.1", "172.17.0.1"})
	for _, c := range checks {
		if c.Check == "bind" && c.Subject == "docker0" {
			t.Errorf("docker0 listed in bind still warned: %+v", checks)
		}
	}

	checks = checkBind(deps, []string{"10.0.0.5"})
	if len(checks) != 2 || checks[0].Status != Fail || checks[0].Subject != "10.0.0.5" {
		t.Errorf("address on no interface: %+v", checks)
	}

	errDeps := Deps{Interfaces: func() ([]Iface, error) { return nil, errors.New("boom") }}
	checks = checkBind(errDeps, []string{"127.0.0.1"})
	if len(checks) != 1 || checks[0].Status != Fail {
		t.Errorf("Interfaces error: %+v", checks)
	}
}

// genCA returns a throwaway self-signed root, its PEM encoding, and the
// parsed certificate and key to sign leaves with.
func genCA(t *testing.T, cn string) ([]byte, *x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), cert, key
}

func TestCheckCA(t *testing.T) {
	rootPEM, _, _ := genCA(t, "doctor test root")

	missing := Deps{ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist }}
	if got := checkCA(missing, "/root.crt", false); got.Status != Warn || got.Fix != "devingress trust" {
		t.Errorf("missing root: %+v", got)
	}
	if got := checkCA(missing, "/root.crt", true); got.Fix != "mkcert -install" {
		t.Errorf("missing mkcert root: %+v", got)
	}

	garbage := Deps{ReadFile: func(string) ([]byte, error) { return []byte("not pem"), nil }}
	if got := checkCA(garbage, "/root.crt", false); got.Status != Warn || !strings.Contains(got.Message, "not valid PEM") {
		t.Errorf("invalid PEM: %+v", got)
	}

	untrusted := Deps{
		ReadFile: func(string) ([]byte, error) { return rootPEM, nil },
		CertPool: func() (*x509.CertPool, error) { return x509.NewCertPool(), nil },
	}
	if got := checkCA(untrusted, "/root.crt", false); got.Status != Warn || !strings.Contains(got.Message, "not trusted") {
		t.Errorf("untrusted root: %+v", got)
	}

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(rootPEM)
	trusted := Deps{
		ReadFile: func(string) ([]byte, error) { return rootPEM, nil },
		CertPool: func() (*x509.CertPool, error) { return pool, nil },
	}
	if got := checkCA(trusted, "/root.crt", false); got.Status != OK {
		t.Errorf("trusted root: %+v", got)
	}
}

func TestCheckOwners(t *testing.T) {
	present := t.TempDir()
	gone := filepath.Join(t.TempDir(), "removed")
	checks := checkOwners([]routefile.Owner{{Dir: present}, {Dir: gone}})
	if len(checks) != 2 || checks[0].Status != OK || checks[1].Status != Warn || checks[1].Subject != gone {
		t.Errorf("owners: %+v", checks)
	}
}

func TestCheckHostDNS(t *testing.T) {
	loopback := Deps{Lookup: func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("127.0.0.1")}, nil }}
	if got := checkHostDNS(loopback, "web.example.dev"); got.Status != OK {
		t.Errorf("loopback: %+v", got)
	}

	public := Deps{Lookup: func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil }}
	if got := checkHostDNS(public, "web.example.dev"); got.Status != Fail || !strings.Contains(got.Message, "not loopback") {
		t.Errorf("non-loopback: %+v", got)
	}

	unresolved := Deps{Lookup: func(string) ([]net.IP, error) { return nil, errors.New("no such host") }}
	if got := checkHostDNS(unresolved, "web.example.dev"); got.Status != Fail || !strings.Contains(got.Message, "does not resolve") {
		t.Errorf("unresolved: %+v", got)
	}
}

func TestCheckHostTLS(t *testing.T) {
	rootPEM, rootCert, rootKey := genCA(t, "doctor test root")
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(rootPEM)

	host := "web.example.dev"
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, rootCert, &leafKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf := tls.Certificate{Certificate: [][]byte{leafDER, rootCert.Raw}, PrivateKey: leafKey}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{leaf}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	deps := DefaultDeps()
	if got := checkHostTLS(deps, host, port, pool, nil); got.Status != OK {
		t.Errorf("verifies: %+v", got)
	}

	otherPEM, _, _ := genCA(t, "a different root")
	otherPool := x509.NewCertPool()
	otherPool.AppendCertsFromPEM(otherPEM)
	if got := checkHostTLS(deps, host, port, otherPool, nil); got.Status != Fail {
		t.Errorf("wrong root: %+v", got)
	}

	if got := checkHostTLS(deps, host, port, nil, errors.New("root unreadable")); got.Status != Fail ||
		!strings.Contains(got.Message, "root unreadable") {
		t.Errorf("root error: %+v", got)
	}
}

func TestCheckHosts(t *testing.T) {
	present := t.TempDir()
	gone := filepath.Join(t.TempDir(), "removed")
	owners := []routefile.Owner{
		{Dir: present, Sites: []routefile.Site{{Host: "web.example.dev"}, {Host: "web.example.dev"}}},
		{Dir: gone, Sites: []routefile.Site{{Host: "api.example.dev"}}},
	}
	deps := Deps{
		ReadFile: func(string) ([]byte, error) { return nil, errors.New("no root") },
		Lookup:   func(string) ([]net.IP, error) { return nil, errors.New("no such host") },
		DialTLS:  func(string, string, *tls.Config) (*tls.Conn, error) { return nil, errors.New("refused") },
	}
	checks := checkHosts(deps, owners, 443, "/root.crt")
	if len(checks) != 2 {
		t.Fatalf("got %d checks, want 2 (dedup, skip gone owner): %+v", len(checks), checks)
	}
	for _, c := range checks {
		if c.Subject != "web.example.dev" {
			t.Errorf("unexpected subject: %+v", c)
		}
	}
}

func TestCheckService(t *testing.T) {
	const exe = "/opt/devingress"

	notInstalled := Deps{ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist }}
	if got := checkService(notInstalled); got != nil {
		t.Errorf("not installed: %+v", got)
	}

	matches := Deps{
		ReadFile: func(name string) ([]byte, error) {
			if name != service.UnitPath {
				t.Fatalf("read %s, want %s", name, service.UnitPath)
			}
			return []byte("[Service]\nExecStart=" + exe + " serve\n"), nil
		},
		Executable: func() (string, error) { return exe, nil },
	}
	got := checkService(matches)
	if len(got) != 1 || got[0].Status != OK {
		t.Errorf("matching ExecStart: %+v", got)
	}

	stale := Deps{
		ReadFile:   func(string) ([]byte, error) { return []byte("[Service]\nExecStart=/old/devingress serve\n"), nil },
		Executable: func() (string, error) { return exe, nil },
	}
	got = checkService(stale)
	if len(got) != 1 || got[0].Status != Warn || !strings.Contains(got[0].Message, "/old/devingress") {
		t.Errorf("stale ExecStart: %+v", got)
	}

	noExecStart := Deps{
		ReadFile:   func(string) ([]byte, error) { return []byte("[Service]\n"), nil },
		Executable: func() (string, error) { return exe, nil },
	}
	got = checkService(noExecStart)
	if len(got) != 1 || got[0].Status != Warn || !strings.Contains(got[0].Message, "no ExecStart") {
		t.Errorf("no ExecStart line: %+v", got)
	}
}

func isolate(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, v := range []string{"XDG_STATE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR"} {
		t.Setenv(v, filepath.Join(root, strings.ToLower(v)))
	}
	return root
}

func TestRunEmptyEnvironment(t *testing.T) {
	isolate(t)
	checks := Run(DefaultDeps())
	if len(checks) == 0 {
		t.Fatal("no checks ran")
	}
	if checks[0].Check != "config" || checks[0].Status != OK {
		t.Errorf("config check: %+v", checks[0])
	}
}

func TestRunBadConfigShortCircuits(t *testing.T) {
	root := isolate(t)
	confDir := filepath.Join(root, "xdg_config_home", "devingress")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "config.toml"), []byte("bind = [not valid toml"), 0o644); err != nil {
		t.Fatal(err)
	}
	checks := Run(DefaultDeps())
	if len(checks) != 1 || checks[0].Check != "config" || checks[0].Status != Fail {
		t.Errorf("checks = %+v, want exactly one failing config check", checks)
	}
}
