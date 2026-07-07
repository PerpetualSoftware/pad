package webhooks

import (
	"fmt"
	"net"
	"net/url"
)

// ValidateWebhookURL checks that a webhook URL is safe to call.
// It rejects non-HTTP(S) schemes, URLs with credentials, private/reserved
// IPs (loopback, link-local, RFC1918, cloud metadata), and hostnames that
// resolve to private IPs.
func ValidateWebhookURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	// Scheme must be http or https
	switch u.Scheme {
	case "http", "https":
		// ok
	default:
		return fmt.Errorf("unsupported scheme %q: only http and https are allowed", u.Scheme)
	}

	// Reject URLs with embedded credentials
	if u.User != nil {
		return fmt.Errorf("URLs with embedded credentials are not allowed")
	}

	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("URL must have a hostname")
	}

	// Check if host is a literal IP
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateIP(ip) {
			return fmt.Errorf("webhook URLs must not target private or reserved IP addresses")
		}
		return nil
	}

	// Host is a name — resolve it and check all resulting IPs
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("failed to resolve hostname %q: %w", host, err)
	}
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return fmt.Errorf("hostname %q resolves to private/reserved IP %s", host, ip)
		}
	}

	return nil
}

// isPrivateIP returns true if the IP is in a private, reserved, or
// otherwise non-routable range. It covers loopback (127.0.0.0/8, ::1),
// RFC1918 (10/8, 172.16/12, 192.168/16) and IPv6 unique-local (fc00::/7)
// via the stdlib predicates, link-local (169.254.0.0/16 — which catches
// the AWS/GCP/Azure metadata IP 169.254.169.254 — and fe80::/10), all
// multicast (224.0.0.0/4, ff00::/8), the unspecified address, and the
// extra reserved ranges in reservedRanges below.
func isPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
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

// reservedRanges are additional blocks that are not routable on the public
// internet but are not caught by the stdlib predicates above. Precomputed
// at init so isPrivateIP stays allocation-free and concurrency-safe.
var reservedRanges = mustParseCIDRs(
	"0.0.0.0/8",          // "this network" (RFC 1122) — some stacks route it locally
	"100.64.0.0/10",      // CGNAT (RFC 6598)
	"192.0.0.0/24",       // IETF protocol assignments (RFC 6890)
	"198.18.0.0/15",      // benchmarking (RFC 2544)
	"240.0.0.0/4",        // reserved / Class E (also contains 255.255.255.255)
	"255.255.255.255/32", // limited broadcast
	"192.0.2.0/24",       // TEST-NET-1 documentation (RFC 5737)
	"198.51.100.0/24",    // TEST-NET-2 documentation (RFC 5737)
	"203.0.113.0/24",     // TEST-NET-3 documentation (RFC 5737)
	"2001:db8::/32",      // IPv6 documentation (RFC 3849)
)

func mustParseCIDRs(networks ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(networks))
	for _, n := range networks {
		_, cidr, err := net.ParseCIDR(n)
		if err != nil {
			panic(fmt.Errorf("webhooks: parse reserved CIDR %q: %w", n, err))
		}
		out = append(out, cidr)
	}
	return out
}
