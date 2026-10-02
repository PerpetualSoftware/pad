// Package netpolicy is the one address policy for server-side outbound
// requests to user-supplied URLs (BUG-3358): webhook delivery and URL import
// both refuse to connect to an address Blocked reports. Each caller checks it
// at DIAL time against the address actually being connected to, so DNS
// answers and redirects are covered as well as literal addresses.
//
// It used to be two lists. URL import blocked only loopback, RFC 1918,
// link-local and CGNAT, so a URL import could reach 198.18.0.0/15, 0.0.0.0/8
// or 240.0.0.0/4 while webhooks refused them, and the import door returns the
// fetched body to the caller.
package netpolicy

import (
	"fmt"
	"net"
)

// Blocked reports whether ip is in a private, reserved or otherwise
// non-public range that a server-side fetch must never connect to.
//
// The stdlib predicates cover loopback (127.0.0.0/8, ::1), RFC 1918 and IPv6
// unique-local (fc00::/7), link-local (169.254.0.0/16, which holds the
// AWS/GCP/Azure metadata address 169.254.169.254, and fe80::/10), multicast
// (224.0.0.0/4, ff00::/8) and the unspecified address; reservedRanges adds
// the rest. An IPv4-mapped IPv6 address (::ffff:a.b.c.d) is judged as its
// IPv4 address, since net.IPNet.Contains and the predicates unwrap it.
func Blocked(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified() ||
		ip.IsPrivate() {
		return true
	}
	for _, cidr := range reservedRanges {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// reservedRanges are blocks not routable on the public internet, or that
// carry an IPv4 address inside an IPv6 one (and so can reach any IPv4 range
// through a translator), that the stdlib predicates do not catch.
// Precomputed at init so Blocked is allocation-free and concurrency-safe.
var reservedRanges = mustParseCIDRs(
	// IPv4
	"0.0.0.0/8",          // "this network" (RFC 1122); some stacks route it locally
	"100.64.0.0/10",      // CGNAT (RFC 6598)
	"192.0.0.0/24",       // IETF protocol assignments (RFC 6890)
	"192.0.2.0/24",       // TEST-NET-1 documentation (RFC 5737)
	"198.18.0.0/15",      // benchmarking (RFC 2544)
	"198.51.100.0/24",    // TEST-NET-2 documentation (RFC 5737)
	"203.0.113.0/24",     // TEST-NET-3 documentation (RFC 5737)
	"240.0.0.0/4",        // reserved / Class E (contains 255.255.255.255)
	"255.255.255.255/32", // limited broadcast
	// IPv6
	"::/96",          // IPv4-compatible (deprecated, RFC 4291)
	"64:ff9b::/96",   // NAT64 well-known prefix (RFC 6052): embeds any IPv4
	"64:ff9b:1::/48", // NAT64 local-use prefix (RFC 8215)
	"100::/64",       // discard-only (RFC 6666)
	"2001::/23",      // IETF protocol assignments, including Teredo 2001::/32
	"2001:db8::/32",  // documentation (RFC 3849)
	"2002::/16",      // 6to4 (RFC 3056): embeds any IPv4
	"fec0::/10",      // site-local (deprecated, RFC 3879)
)

func mustParseCIDRs(networks ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(networks))
	for _, n := range networks {
		_, cidr, err := net.ParseCIDR(n)
		if err != nil {
			panic(fmt.Errorf("netpolicy: parse reserved CIDR %q: %w", n, err))
		}
		out = append(out, cidr)
	}
	return out
}
