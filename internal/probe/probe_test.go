package probe

import (
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestTarget(t *testing.T) {
	for _, tc := range []struct {
		upstream, network, address string
		ok                         bool
	}{
		{"127.0.0.1:15766", "tcp", "127.0.0.1:15766", true},
		{"host.docker.internal:3000", "tcp", "host.docker.internal:3000", true},
		{"[::1]:3000", "tcp", "[::1]:3000", true},
		{"http://example.internal", "tcp", "example.internal:80", true},
		{"https://example.internal", "tcp", "example.internal:443", true},
		{"h2c://127.0.0.1:9000", "tcp", "127.0.0.1:9000", true},
		{"http://[::1]", "tcp", "[::1]:80", true},
		{"unix//run/app.sock", "unix", "/run/app.sock", true},
		{"{http.request.header.X-Upstream}", "", "", false},
		{"127.0.0.1:{env.PORT}", "", "", false},
		{":3000-3005", "", "", false},
		{"127.0.0.1:3000 127.0.0.1:3001", "", "", false},
		{"localhost", "", "", false},
		{"ftp://example.internal:21", "", "", false},
		{"http://example.internal/api", "", "", false},
		{"", "", "", false},
	} {
		network, address, ok := Target(tc.upstream)
		if network != tc.network || address != tc.address || ok != tc.ok {
			t.Errorf("Target(%q) = %q, %q, %v; want %q, %q, %v",
				tc.upstream, network, address, ok, tc.network, tc.address, tc.ok)
		}
	}
}

func TestAllRealListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
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
			c.Close()
		}
	}()
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	closed.Close()

	up, down := ln.Addr().String(), closedAddr
	got := All(net.DialTimeout, []string{up, down, "{placeholder}"})
	if got[up] != Up || got[down] != Down || got["{placeholder}"] != Unknown || len(got) != 3 {
		t.Errorf("got %v; want %s up, %s down, placeholder unknown", got, up, down)
	}
}

func TestAllDialsEachUpstreamOnce(t *testing.T) {
	var dials atomic.Int32
	dial := func(network, address string, timeout time.Duration) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("refused")
	}
	got := All(dial, []string{"127.0.0.1:1", "127.0.0.1:1", "127.0.0.1:2", ":3000-3005"})
	if n := dials.Load(); n != 2 {
		t.Errorf("%d dials, want 2", n)
	}
	if got["127.0.0.1:1"] != Down || got["127.0.0.1:2"] != Down || got[":3000-3005"] != Unknown {
		t.Errorf("got %v", got)
	}
}

func TestAllIsConcurrent(t *testing.T) {
	dial := func(network, address string, timeout time.Duration) (net.Conn, error) {
		time.Sleep(timeout)
		return nil, errors.New("i/o timeout")
	}
	var upstreams []string
	for i := range 24 {
		upstreams = append(upstreams, fmt.Sprintf("127.0.0.1:%d", 10000+i))
	}
	start := time.Now()
	All(dial, upstreams)
	if took := time.Since(start); took > 3*Timeout {
		t.Errorf("24 timed-out dials took %v; want about one Timeout (%v)", took, Timeout)
	}
}
