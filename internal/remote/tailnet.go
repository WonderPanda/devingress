package remote

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// Iface is one network interface's name and addresses.
type Iface struct {
	Name  string
	Addrs []net.IP
}

// Interfaces lists this machine's interfaces and their addresses.
func Interfaces() ([]Iface, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]Iface, 0, len(ifs))
	for _, ifc := range ifs {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		var ips []net.IP
		for _, a := range addrs {
			if ipNet, ok := a.(*net.IPNet); ok {
				ips = append(ips, ipNet.IP)
			}
		}
		out = append(out, Iface{Name: ifc.Name, Addrs: ips})
	}
	return out, nil
}

// tailnet is the CGNAT range Tailscale assigns node addresses from.
var tailnet = netip.MustParsePrefix("100.64.0.0/10")

// TailnetIPv4 finds this machine's tailnet address without the tailscale CLI (launchd's PATH does
// not have it): the one IPv4 in 100.64.0.0/10 on interface tailscale0 on Linux, or on a utun
// interface on macOS. None, or several, is an error saying so.
func TailnetIPv4(ifaces []Iface, goos string) (netip.Addr, error) {
	where := "interface tailscale0"
	match := func(name string) bool { return name == "tailscale0" }
	if goos == "darwin" {
		where = "any utun interface"
		match = func(name string) bool { return strings.HasPrefix(name, "utun") }
	}
	var found []netip.Addr
	var desc []string
	for _, ifc := range ifaces {
		if !match(ifc.Name) {
			continue
		}
		for _, ip := range ifc.Addrs {
			a, ok := netip.AddrFromSlice(ip)
			if !ok {
				continue
			}
			if a = a.Unmap(); a.Is4() && tailnet.Contains(a) {
				found = append(found, a)
				desc = append(desc, a.String()+" on "+ifc.Name)
			}
		}
	}
	switch len(found) {
	case 0:
		return netip.Addr{}, fmt.Errorf("no tailnet address (100.64.0.0/10) on %s: is Tailscale running and logged in?", where)
	case 1:
		return found[0], nil
	}
	return netip.Addr{}, fmt.Errorf("several tailnet addresses (%s): cannot tell which one other devices reach", strings.Join(desc, ", "))
}
