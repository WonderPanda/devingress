package ingress

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"syscall"

	"github.com/caddyserver/caddy/v2"

	"github.com/WonderPanda/devingress/internal/config"
)

// macOS lets a non-root process bind a port below 1024 only on the wildcard address, so serve
// listens there for a loopback bind address, with the socket bound to lo0 so only loopback
// traffic reaches it.
func init() { caddy.RegisterNetwork(config.LoopbackNetwork, listenLoopback) }

// listenLoopback listens on the wildcard address of host's family at the port, accepting only
// connections that arrive on lo0. SO_REUSEPORT lets a reload's new listener overlap the old one.
func listenLoopback(ctx context.Context, _, host, portRange string, portOffset uint, cfg net.ListenConfig) (any, error) {
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("%s listens only on a loopback address, not %q", config.LoopbackNetwork, host)
	}
	start, _, _ := strings.Cut(portRange, "-")
	port, err := strconv.ParseUint(start, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("%s: port %q: %w", config.LoopbackNetwork, portRange, err)
	}
	lo0, err := net.InterfaceByName("lo0")
	if err != nil {
		return nil, err
	}
	network, wildcard, level, boundIf := "tcp4", "0.0.0.0", syscall.IPPROTO_IP, syscall.IP_BOUND_IF
	if ip.To4() == nil {
		network, wildcard, level, boundIf = "tcp6", "::", syscall.IPPROTO_IPV6, syscall.IPV6_BOUND_IF
	}

	control := cfg.Control
	cfg.Control = func(network, address string, c syscall.RawConn) error {
		if control != nil {
			if err := control(network, address, c); err != nil {
				return err
			}
		}
		var sockErr error
		err := c.Control(func(fd uintptr) {
			for _, o := range []struct{ level, name, value int }{
				{level, boundIf, lo0.Index},
				{syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1},
				{syscall.SOL_SOCKET, syscall.SO_REUSEPORT, 1},
			} {
				if sockErr = syscall.SetsockoptInt(int(fd), o.level, o.name, o.value); sockErr != nil {
					return
				}
			}
		})
		if err != nil {
			return err
		}
		return sockErr
	}
	return cfg.Listen(ctx, network, net.JoinHostPort(wildcard, strconv.FormatUint(port+uint64(portOffset), 10)))
}
