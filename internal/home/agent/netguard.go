package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// The proxy resolves names itself and refuses to connect to addresses that are not on the public Internet:
// otherwise an allowed name (or a name an attacker controls that the list lets through) could point at the
// Mac, the home network or a cloud metadata address. The check runs on the resolved addresses before dialing
// and again, through Dialer.Control, on the address the socket actually connects to.

var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",      // "this network", includes 0.0.0.0
		"100.64.0.0/10",  // carrier-grade NAT (also Tailscale)
		"192.0.0.0/24",   // IETF protocol assignments
		"198.18.0.0/15",  // benchmarking
		"240.0.0.0/4",    // reserved and broadcast
		"64:ff9b::/96",   // NAT64: embeds an IPv4 address
		"64:ff9b:1::/48", // local-use NAT64
		"2002::/16",      // 6to4: embeds an IPv4 address
		"100::/64",       // discard-only
		"fec0::/10",      // deprecated site-local
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// blockedAddr reports addresses the proxy never connects to (unless the host is exempt).
func blockedAddr(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func blockedIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	return !ok || blockedAddr(a)
}

var errBlockedAddr = errors.New("the address is not on the public Internet")

// guardControl is a Dialer.Control that rejects a connection to a blocked address.
func guardControl(network, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: %q", errBlockedAddr, address)
	}
	if blockedAddr(ap.Addr()) {
		return fmt.Errorf("%w: %s", errBlockedAddr, ap.Addr())
	}
	return nil
}

func defaultResolve(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.IP)
	}
	return out, nil
}

func defaultDial(guard bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 15 * time.Second}
	if guard {
		d.Control = guardControl
	}
	return d.DialContext
}
