//go:build e2e

// Package e2e builds the real devingress binary, runs `serve` fully isolated
// from any real instance (every XDG dir in a temp root, unprivileged ports),
// and drives it over real TLS.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	domain = "di-test.example"
	hello  = "hello, world\n"
)

var (
	bin      string    // the built devingress binary
	srv      *instance // shared by every case but TestCaddyOwnRoot
	upstream *httptest.Server
	caDir    string // the throwaway root srv signs with
	mainDir  string // the checkout registered by TestMain, on branch e2e-branch
)

func TestMain(m *testing.M) {
	code, err := setup(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e setup:", err)
		code = 1
	}
	os.Exit(code)
}

func setup(m *testing.M) (int, error) {
	// Short root: the admin socket path must fit in 107 bytes.
	root, err := os.MkdirTemp("", "di")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(root)

	bin = filepath.Join(root, "devingress")
	build := exec.Command("go", "build", "-o", bin, "github.com/WonderPanda/devingress/cmd/devingress")
	if out, err := build.CombinedOutput(); err != nil {
		return 0, fmt.Errorf("go build: %v\n%s", err, out)
	}

	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, hello)
	}))
	defer upstream.Close()

	caDir = filepath.Join(root, "ca")
	if err := writeCA(caDir); err != nil {
		return 0, err
	}
	srv, err = startInstance(filepath.Join(root, "main"), caDir, "")
	if err != nil {
		return 0, err
	}
	defer srv.stop()

	mainDir = filepath.Join(root, "checkout")
	err = newCheckout(srv, mainDir, "e2e-branch", fmt.Sprintf(`
domain = %q

[[route]]
host = "web-${slug}"
to = "%s"

[[route]]
host = "h1-${slug}"
to = "%[2]s"
protocols = ["h1"]
`, domain, upstreamPort()))
	if err != nil {
		return 0, err
	}
	if out, err := srv.run(mainDir, "up"); err != nil {
		return 0, fmt.Errorf("up: %v\n%s\n--- devingress serve log ---\n%s", err, out, srv.log.String())
	}

	code := m.Run()
	if code != 0 {
		fmt.Fprintf(os.Stderr, "--- devingress serve log ---\n%s", srv.log.String())
	}
	return code, nil
}

// instance is one isolated `devingress serve`.
type instance struct {
	root      string
	env       []string
	httpsPort int
	dnsAddr   string // dns_listen
	cmd       *exec.Cmd
	log       *syncBuffer
	exited    chan struct{}
}

// startInstance runs `serve` with every XDG dir under root. caRoot == ""
// leaves ca_root unset so Caddy mints its own root; dnsAddr == "" picks a free
// port for dns_listen.
func startInstance(root, caRoot, dnsAddr string) (*instance, error) {
	in := &instance{root: root, log: &syncBuffer{}, exited: make(chan struct{})}
	httpPort, err := freePort()
	if err != nil {
		return nil, err
	}
	if in.httpsPort, err = freePort(); err != nil {
		return nil, err
	}
	if in.dnsAddr = dnsAddr; dnsAddr == "" {
		dnsPort, err := freePort()
		if err != nil {
			return nil, err
		}
		in.dnsAddr = fmt.Sprintf("127.0.0.1:%d", dnsPort)
	}

	xdg := map[string]string{
		"XDG_STATE_HOME":  filepath.Join(root, "state"),
		"XDG_CONFIG_HOME": filepath.Join(root, "config"),
		"XDG_DATA_HOME":   filepath.Join(root, "data"),
		"XDG_RUNTIME_DIR": filepath.Join(root, "run"),
		// HOME too, so nothing (Caddy, git, `mkcert -CAROOT`) falls back to the real one.
		"HOME": filepath.Join(root, "home"),
	}
	for k, v := range xdg {
		if err := os.MkdirAll(v, 0o700); err != nil {
			return nil, err
		}
		in.env = append(in.env, k+"="+v)
	}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if _, ours := xdg[k]; !ours && k != "CAROOT" && !strings.HasPrefix(k, "GIT_") {
			in.env = append(in.env, kv)
		}
	}

	conf := fmt.Sprintf("bind = [\"127.0.0.1\"]\nhttp_port = %d\nhttps_port = %d\ndns_listen = %q\n",
		httpPort, in.httpsPort, in.dnsAddr)
	if caRoot != "" {
		conf += fmt.Sprintf("ca_root = %q\n", caRoot)
	}
	confDir := filepath.Join(xdg["XDG_CONFIG_HOME"], "devingress")
	if err := os.MkdirAll(confDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(confDir, "config.toml"), []byte(conf), 0o600); err != nil {
		return nil, err
	}

	in.cmd = exec.Command(bin, "serve")
	in.cmd.Env = in.env
	in.cmd.Stdout, in.cmd.Stderr = in.log, in.log
	// If the test binary dies without running stop, take serve with it.
	in.cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := in.cmd.Start(); err != nil {
		return nil, err
	}
	go func() { in.cmd.Wait(); close(in.exited) }()

	sock := filepath.Join(xdg["XDG_RUNTIME_DIR"], "devingress", "admin.sock")
	err = poll(30*time.Second, func() error {
		select {
		case <-in.exited:
			return errStop{fmt.Errorf("serve exited: %v\n%s", in.cmd.ProcessState, in.log.String())}
		default:
		}
		c, err := net.Dial("unix", sock)
		if err == nil {
			c.Close()
		}
		return err
	})
	if err != nil {
		in.stop()
		return nil, err
	}
	return in, nil
}

func (in *instance) stop() {
	select {
	case <-in.exited:
		return
	default:
	}
	in.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-in.exited:
	case <-time.After(10 * time.Second):
		in.cmd.Process.Kill()
		<-in.exited
	}
}

// run runs a devingress subcommand in dir with the instance's env.
func (in *instance) run(dir string, args ...string) (string, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir, cmd.Env = dir, in.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// newCheckout makes a git checkout on branch (one commit, so ${slug}
// resolves) holding routes as its devingress.toml.
func newCheckout(in *instance, dir, branch, routes string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", branch},
		{"-c", "user.name=e2e", "-c", "user.email=e2e@" + domain, "-c", "commit.gpgsign=false",
			"commit", "-q", "--allow-empty", "-m", "e2e"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = dir, append(in.env, "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %v\n%s", args, err, out)
		}
	}
	return os.WriteFile(filepath.Join(dir, "devingress.toml"), []byte(routes), 0o644)
}

// checkout is newCheckout under a per-test dir that is removed afterwards.
func checkout(t *testing.T, in *instance, branch, routes string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), branch)
	if err := newCheckout(in, dir, branch, routes); err != nil {
		t.Fatal(err)
	}
	return dir
}

func up(t *testing.T, in *instance, dir string) string {
	t.Helper()
	out, err := in.run(dir, "up")
	if err != nil {
		t.Fatalf("up %s: %v\n%s", dir, err, out)
	}
	return out
}

// client resolves every host to the instance's HTTPS port and trusts only roots.
// No keep-alive, so every request is a fresh handshake against the live config.
func (in *instance) client(roots *x509.CertPool) *http.Client {
	addr := fmt.Sprintf("127.0.0.1:%d", in.httpsPort)
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
			TLSClientConfig:   &tls.Config{RootCAs: roots},
			DisableKeepAlives: true,
		},
	}
}

// serves reports nil when https://host/ returns the upstream's body with 200.
func (in *instance) serves(roots *x509.CertPool, host string) error {
	resp, err := in.client(roots).Get("https://" + host + "/")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK || string(body) != hello {
		return fmt.Errorf("%s: got %s %q", host, resp.Status, body)
	}
	return nil
}

// waitServes polls until host serves: Caddy reloads asynchronously and
// issuing a leaf takes a moment.
func (in *instance) waitServes(t *testing.T, roots *x509.CertPool, host string) {
	t.Helper()
	if err := poll(30*time.Second, func() error { return in.serves(roots, host) }); err != nil {
		t.Fatalf("%s never served: %v", host, err)
	}
}

// waitGone polls until host stops serving, then checks a fresh request fails too.
func (in *instance) waitGone(t *testing.T, roots *x509.CertPool, host string) {
	t.Helper()
	err := poll(30*time.Second, func() error {
		if in.serves(roots, host) == nil {
			return fmt.Errorf("%s still serves", host)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if in.serves(roots, host) == nil {
		t.Fatalf("%s served again after it stopped", host)
	}
}

// handshake does a bare TLS handshake for host, offering h2 and http/1.1.
func (in *instance) handshake(roots *x509.CertPool, host string) (tls.ConnectionState, error) {
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp",
		fmt.Sprintf("127.0.0.1:%d", in.httpsPort),
		&tls.Config{ServerName: host, RootCAs: roots, NextProtos: []string{"h2", "http/1.1"}})
	if err != nil {
		return tls.ConnectionState{}, err
	}
	defer conn.Close()
	return conn.ConnectionState(), nil
}

// errStop ends a poll early.
type errStop struct{ error }

func poll(timeout time.Duration, f func() error) error {
	deadline := time.Now().Add(timeout)
	for {
		err := f()
		if err == nil {
			return nil
		}
		var stop errStop
		if errors.As(err, &stop) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func upstreamPort() string {
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	return port
}

// writeCA writes a throwaway root shaped like mkcert's (RSA-3072, PKCS#8 key,
// pathlen:0) to dir.
func writeCA(dir string) error {
	key, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{Organization: []string{"devingress e2e"}, CommonName: "devingress e2e root"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "rootCA.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "rootCA-key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600)
}

// readCert reads a PEM certificate file.
func readCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatalf("%s: no PEM block", path)
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return c
}

// pool is a cert pool holding only the certificate at path.
func pool(t *testing.T, path string) *x509.CertPool {
	t.Helper()
	p := x509.NewCertPool()
	p.AddCert(readCert(t, path))
	return p
}

// syncBuffer collects serve's output while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
