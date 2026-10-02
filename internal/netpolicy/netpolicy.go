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
	// Anything that is not a well-formed 4- or 16-byte address is refused:
	// the predicates below answer false for a malformed slice, which would
	// fail open.
	if len(ip) != net.IPv4len && len(ip) != net.IPv6len {
		return true
	}
	// NAT64 well-known prefix (RFC 6052): the address names the IPv4 host in
	// its last 32 bits, which may be public (an IPv6-only deployment reaches
	// IPv4-only hosts this way). Judge the embedded IPv4 address instead.
	if len(ip) == net.IPv6len && nat64WellKnown.Contains(ip) && ip.To4() == nil {
		return Blocked(net.IP(ip[12:16]))
	}
	for _, cidr := range publicExceptions {
		if cidr.Contains(ip) {
			return false
		}
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
	"192.0.0.0/24",       // IETF protocol assignments (RFC 6890); public exceptions above
	"192.88.99.0/24",     // deprecated 6to4 relay anycast (RFC 7526)
	"192.0.2.0/24",       // TEST-NET-1 documentation (RFC 5737)
	"198.18.0.0/15",      // benchmarking (RFC 2544)
	"198.51.100.0/24",    // TEST-NET-2 documentation (RFC 5737)
	"203.0.113.0/24",     // TEST-NET-3 documentation (RFC 5737)
	"240.0.0.0/4",        // reserved / Class E (contains 255.255.255.255)
	"255.255.255.255/32", // limited broadcast
	// IPv6
	"::/96",          // IPv4-compatible (deprecated, RFC 4291)
	"64:ff9b:1::/48", // NAT64 local-use prefix (RFC 8215): network-specific embedding, unknowable here
	"100::/64",       // discard-only (RFC 6666)
	"100:0:0:1::/64", // dummy IPv6 prefix (RFC 9780)
	"2001::/23",      // IETF protocol assignments, incl. Teredo 2001::/32; public exceptions above
	"2001:db8::/32",  // documentation (RFC 3849)
	"2002::/16",      // 6to4 (RFC 3056): embeds any IPv4
	"3fff::/20",      // documentation (RFC 9637)
	"5f00::/16",      // SRv6 SIDs (RFC 9602)
	"fec0::/10",      // site-local (deprecated, RFC 3879)
)

// nat64WellKnown is RFC 6052's well-known prefix; Blocked judges the IPv4
// address it embeds rather than the prefix.
var nat64WellKnown = mustParseCIDRs("64:ff9b::/96")[0]

// publicExceptions are blocks inside a reserved range above that the IANA
// special-purpose registries mark globally reachable, so they are allowed.
var publicExceptions = mustParseCIDRs(
	"192.0.0.9/32",    // Port Control Protocol anycast (RFC 7723)
	"192.0.0.10/32",   // TURN relay anycast (RFC 8155)
	"2001:1::1/128",   // Port Control Protocol anycast (RFC 7723)
	"2001:1::2/128",   // TURN relay anycast (RFC 8155)
	"2001:1::3/128",   // DNS-SD service registration anycast (RFC 9665)
	"2001:3::/32",     // AMT (RFC 7450)
	"2001:4:112::/48", // AS112-v6 (RFC 7535)
	"2001:20::/28",    // ORCHIDv2 (RFC 7343)
	"2001:30::/28",    // drone remote ID DETs (RFC 9374)
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
