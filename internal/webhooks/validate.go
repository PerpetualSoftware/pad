package webhooks

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"

	"github.com/PerpetualSoftware/pad/internal/netpolicy"
)

// ResolveError is a hostname lookup that failed (BUG-3661): a timeout, a
// SERVFAIL, an unreachable resolver. It says nothing about whether the URL is
// allowed, so a delivery that hits one is transient, not refused; the policy's
// own refusals are plain errors.
type ResolveError struct {
	Host string
	Err  error
}

func (e *ResolveError) Error() string {
	return fmt.Sprintf("failed to resolve hostname %q: %v", e.Host, e.Err)
}

func (e *ResolveError) Unwrap() error { return e.Err }

// IsResolveError reports whether err is, or wraps, a ResolveError.
func IsResolveError(err error) bool {
	var re *ResolveError
	return errors.As(err, &re)
}

// lookupIP resolves a webhook hostname; a test replaces it.
var lookupIP = net.DefaultResolver.LookupIP

// ValidateWebhookURL checks that a webhook URL is safe to call.
// It rejects non-HTTP(S) schemes, URLs with credentials, private/reserved
// IPs (loopback, link-local, RFC1918, cloud metadata), and hostnames that
// resolve to private IPs.
func ValidateWebhookURL(rawURL string) error {
	return ValidateWebhookURLContext(context.Background(), rawURL)
}

// ValidateWebhookURLContext is ValidateWebhookURL with the hostname lookup
// bound to ctx. A lookup that fails returns a *ResolveError.
func ValidateWebhookURLContext(ctx context.Context, rawURL string) error {
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
	ips, err := lookupIP(ctx, "ip", host)
	if err != nil {
		return &ResolveError{Host: host, Err: err}
	}
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return fmt.Errorf("hostname %q resolves to private/reserved IP %s", host, ip)
		}
	}

	return nil
}

// isPrivateIP reports whether a webhook may not be delivered to ip. The
// policy is the one shared with URL import (BUG-3358): see netpolicy.Blocked.
func isPrivateIP(ip net.IP) bool {
	return netpolicy.Blocked(ip)
}
