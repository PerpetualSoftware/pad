package webhooks

import (
	"fmt"
	"net"
	"net/url"
	"strings"
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
// otherwise non-routable range.
func isPrivateIP(ip net.IP) bool {
	// Loopback (127.0.0.0/8, ::1)
	if ip.IsLoopback() {
		return true
	}

	// Link-local (169.254.0.0/16, fe80::/10)
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}

	// Unspecified (0.0.0.0, ::)
	if ip.IsUnspecified() {
		return true
	}

	// RFC1918 private ranges
	privateRanges := []struct {
		network string
	}{
		{"10.0.0.0/8"},
		{"172.16.0.0/12"},
		{"192.168.0.0/16"},
		// IPv6 unique local (fc00::/7)
		{"fc00::/7"},
		// Cloud metadata (AWS, GCP, Azure)
		{"169.254.169.254/32"},
	}

	for _, r := range privateRanges {
		_, cidr, err := net.ParseCIDR(r.network)
		if err != nil {
			continue
		}
		if cidr.Contains(ip) {
			return true
		}
	}

	// Also catch common cloud metadata IPv6 variants
	if strings.EqualFold(ip.String(), "fd00::") {
		return true
	}

	return false
}
