package remote

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/libdns/cloudflare"
	"github.com/libdns/libdns"
)

// DNSProvider is the part of libdns's Cloudflare provider the wildcard record needs.
type DNSProvider interface {
	libdns.ZoneLister
	libdns.RecordSetter
}

// SetWildcard points the A record *.<suffix> at ip, DNS-only, in the Cloudflare zone that holds
// suffix, creating it when it is missing. It touches no other record: libdns's SetRecords looks
// the record up by type and name and updates only that one.
func SetWildcard(ctx context.Context, p DNSProvider, suffix string, ip netip.Addr) error {
	zones, err := p.ListZones(ctx)
	if err != nil {
		return fmt.Errorf("list zones: %w", err)
	}
	zone := ""
	for _, z := range zones {
		name := strings.TrimSuffix(z.Name, ".")
		if (suffix == name || strings.HasSuffix(suffix, "."+name)) && len(name) > len(zone) {
			zone = name
		}
	}
	if zone == "" {
		return fmt.Errorf("no Cloudflare zone the token can read holds %s (it needs Zone:Read and DNS:Edit on that zone)", suffix)
	}
	name := "*"
	if suffix != zone {
		name = "*." + strings.TrimSuffix(suffix, "."+zone)
	}
	if _, err := p.SetRecords(ctx, zone, []libdns.Record{libdns.Address{Name: name, IP: ip}}); err != nil {
		return fmt.Errorf("set %s.%s A %s: %w", name, zone, ip, err)
	}
	return nil
}

// Updater keeps *.<Suffix> pointed at this machine's tailnet address while serve runs: at start,
// then on every tick whose address differs from the one it last set. No address yet, or an API
// failure, is logged once and retried on the next tick; serving carries on regardless.
type Updater struct {
	Suffix     string
	TokenFile  string
	Interfaces func() ([]Iface, error)
	GOOS       string
	HTTPClient cloudflare.HTTPClient // nil: http.DefaultClient
	Logf       func(format string, args ...any)

	set      netip.Addr // the address the record was last set to
	lastFail string     // the failure last logged, so a repeat is not logged again
}

// Interval is how often Run checks the tailnet address.
const Interval = 3 * time.Minute

// Run calls Tick now and then every Interval until ctx is done.
func (u *Updater) Run(ctx context.Context) {
	t := time.NewTicker(Interval)
	defer t.Stop()
	for {
		u.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick sets the record when the tailnet address differs from the one last set.
func (u *Updater) Tick(ctx context.Context) {
	ifaces, err := u.Interfaces()
	if err != nil {
		u.fail("list interfaces: "+err.Error(), "")
		return
	}
	ip, err := TailnetIPv4(ifaces, u.GOOS)
	if err != nil {
		u.fail(err.Error()+"; *."+u.Suffix+" is not updated until there is one", "")
		return
	}
	if ip == u.set {
		return
	}
	token, err := ReadToken(u.TokenFile)
	if err != nil {
		u.fail(err.Error(), "")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	p := &cloudflare.Provider{APIToken: token, HTTPClient: u.HTTPClient}
	if err := SetWildcard(ctx, p, u.Suffix, ip); err != nil {
		u.fail(err.Error()+"; retrying in "+Interval.String(), token)
		return
	}
	u.set, u.lastFail = ip, ""
	u.Logf("*.%s points at %s (DNS-only)", u.Suffix, ip)
}

func (u *Updater) fail(msg, token string) {
	msg = Redact(msg, token)
	if msg != u.lastFail {
		u.Logf("%s", msg)
		u.lastFail = msg
	}
}
