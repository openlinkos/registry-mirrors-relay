package upstream

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"syscall"
)

// Besides the private, loopback, link-local and multicast addresses netip can detect directly, also block
// these ranges.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // this network
	netip.MustParsePrefix("100.64.0.0/10"),   // CGNAT
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("198.18.0.0/15"),   // testing
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64, may map to private IPv4
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001:db8::/32"), // documentation
}

// Blocked reports whether upstream connections must not go to this address. Upstreams should only be on the
// public internet: blocking private addresses means that even if redirect or token service addresses are
// tampered with, they cannot reach services on this machine or the private network.
func Blocked(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsUnspecified() || a.IsLoopback() || a.IsPrivate() ||
		a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() ||
		a.IsMulticast() || !a.IsGlobalUnicast() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// guardControl checks the address after DNS resolution and before the connection is actually made, so DNS
// rebinding cannot get around it either.
func guardControl(allow func(netip.Addr) bool) func(ctx context.Context, network, address string, c syscall.RawConn) error {
	return func(_ context.Context, _ string, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		a, err := netip.ParseAddr(host)
		if err != nil {
			return fmt.Errorf("cannot parse address %q", host)
		}
		if allow != nil && allow(a) {
			return nil
		}
		if Blocked(a) {
			return &BlockedError{Addr: a}
		}
		return nil
	}
}

// BlockedError means the connection was blocked.
type BlockedError struct{ Addr netip.Addr }

func (e *BlockedError) Error() string {
	return "refusing to dial non-public address " + e.Addr.String()
}
