package ingress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"

	"github.com/WonderPanda/devingress/internal/config"
	"github.com/WonderPanda/devingress/internal/render"
	"github.com/WonderPanda/devingress/internal/routefile"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func listenLo0(t *testing.T, host string, port int) (net.Listener, error) {
	t.Helper()
	ln, err := listenLoopback(context.Background(), config.LoopbackNetwork, host, fmt.Sprint(port), 0, net.ListenConfig{})
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { ln.(net.Listener).Close() })
	return ln.(net.Listener), nil
}

// dialAccept dials addr and checks that ln accepts the connection.
func dialAccept(t *testing.T, ln net.Listener, addr string) {
	t.Helper()
	accepted := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Close()
		}
		accepted <- err
	}()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	c.Close()
	select {
	case err := <-accepted:
		if err != nil {
			t.Fatalf("accept: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("%s listener never accepted the connection", ln.Addr())
	}
}

func TestLoopbackNetworkOverlaps(t *testing.T) {
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	first, err := listenLo0(t, "127.0.0.1", port)
	if err != nil {
		t.Fatal(err)
	}
	if got := first.Addr().String(); got != fmt.Sprintf("0.0.0.0:%d", port) {
		t.Errorf("listening on %s, want the wildcard address", got)
	}
	dialAccept(t, first, addr)

	// A reload listens again before the old config lets go of the port.
	second, err := listenLo0(t, "127.0.0.1", port)
	if err != nil {
		t.Fatalf("second, overlapping listen on %d: %v", port, err)
	}
	first.Close()
	dialAccept(t, second, addr)

	if _, err := listenLo0(t, "192.168.1.10", port); err == nil {
		t.Error("listened for a non-loopback host")
	}
}

// TestLoopbackNetworkPort80 is the assumption the macOS support rests on: a non-root process
// binds port 80 through the network and accepts on 127.0.0.1:80.
func TestLoopbackNetworkPort80(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, which may bind port 80 anyway")
	}
	ln, err := listenLo0(t, "127.0.0.1", 80)
	if errors.Is(err, syscall.EADDRINUSE) {
		t.Skip("port 80 is in use")
	}
	if err != nil {
		t.Fatalf("non-root listen on :80 through %s: %v", config.LoopbackNetwork, err)
	}
	dialAccept(t, ln, "127.0.0.1:80")
}

// TestLoopbackNetworkReload runs Caddy on the network and reloads it, as up and down do.
func TestLoopbackNetworkReload(t *testing.T) {
	sockDir, err := os.MkdirTemp("/tmp", "devingress") // t.TempDir is too long for sun_path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	upstream := func(body string) string {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, body) }))
		t.Cleanup(s.Close)
		return s.Listener.Addr().String()
	}
	opts := render.Options{
		AdminSocket: filepath.Join(sockDir, "admin.sock"),
		StorageDir:  t.TempDir(),
		Bind:        []string{config.LoopbackNetwork + "/127.0.0.1"},
		HTTPPort:    freePort(t),
		HTTPSPort:   freePort(t),
	}
	load := func(upstream string) {
		t.Helper()
		text, err := render.Caddyfile(opts, []routefile.Owner{{Dir: "/src/app",
			Paths: []routefile.PathRoute{{Prefix: "/app", Upstream: upstream}}}})
		if err != nil {
			t.Fatal(err)
		}
		cfgJSON, err := render.Adapt(text)
		if err != nil {
			t.Fatal(err)
		}
		if err := caddy.Load(cfgJSON, true); err != nil {
			t.Fatalf("load: %v", err)
		}
	}
	get := func(want string) {
		t.Helper()
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/app/", opts.HTTPPort))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if body, _ := io.ReadAll(resp.Body); string(body) != want {
			t.Errorf("got %d %q, want %q", resp.StatusCode, body, want)
		}
	}
	t.Cleanup(func() { caddy.Stop() })

	load(upstream("first"))
	get("first")
	load(upstream("second"))
	get("second")
}
