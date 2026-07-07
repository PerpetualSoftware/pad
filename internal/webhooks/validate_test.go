package webhooks

import (
	"net"
	"testing"
)

func TestValidateWebhookURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		// Valid URLs
		{"valid https", "https://example.com/webhook", false},
		{"valid http", "http://example.com/callback", false},
		{"valid with port", "https://example.com:8080/hook", false},
		{"valid with path", "https://example.com/api/v1/webhook", false},

		// Invalid schemes
		{"ftp scheme", "ftp://example.com/hook", true},
		{"javascript scheme", "javascript:alert(1)", true},
		{"file scheme", "file:///etc/passwd", true},
		{"no scheme", "example.com/hook", true},

		// Embedded credentials
		{"with credentials", "https://user:pass@example.com/hook", true},

		// Private IPs
		{"loopback IPv4", "http://127.0.0.1/hook", true},
		{"loopback IPv6", "http://[::1]/hook", true},
		{"private 10.x", "http://10.0.0.1/hook", true},
		{"private 172.16.x", "http://172.16.0.1/hook", true},
		{"private 192.168.x", "http://192.168.1.1/hook", true},
		{"cloud metadata", "http://169.254.169.254/latest/meta-data/", true},
		{"link-local", "http://169.254.1.1/hook", true},
		{"unspecified", "http://0.0.0.0/hook", true},

		// Widened reserved ranges
		{"cgnat 100.64.x", "http://100.64.1.2/hook", true},
		{"ietf protocol 192.0.0.x", "http://192.0.0.8/hook", true},
		{"benchmarking 198.18.x", "http://198.19.0.1/hook", true},
		{"reserved class E 240.x", "http://240.0.0.1/hook", true},
		{"broadcast", "http://255.255.255.255/hook", true},

		// Public addresses just outside the widened ranges must still pass
		{"public above cgnat", "http://100.128.0.1/hook", false},
		{"public above 198.18/15", "http://198.20.0.1/hook", false},

		// Hostnames resolving to private IPs
		{"localhost", "http://localhost/hook", true},

		// Empty/invalid
		{"empty url", "", true},
		{"no host", "http:///path", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateWebhookURL(tt.url)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateWebhookURL(%q) error = %v, wantErr = %v", tt.url, err, tt.wantErr)
			}
		})
	}
}

func TestIsPrivateIP(t *testing.T) {
	tests := []struct {
		ip      string
		private bool
	}{
		{"127.0.0.1", true},
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"192.168.0.1", true},
		{"169.254.169.254", true},
		{"0.0.0.0", true},
		{"::1", true},
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"93.184.216.34", false},
		// Widened reserved ranges
		{"100.64.0.1", true},      // CGNAT lower bound
		{"100.127.255.254", true}, // CGNAT upper bound
		{"192.0.0.1", true},       // IETF protocol assignments
		{"198.18.0.1", true},      // benchmarking lower bound
		{"198.19.255.254", true},  // benchmarking upper bound
		{"240.0.0.1", true},       // reserved / Class E
		{"255.255.255.255", true}, // limited broadcast
		{"0.0.0.1", true},         // "this network" (RFC 1122), not just 0.0.0.0
		{"0.255.255.255", true},   // "this network" upper bound
		{"224.0.0.1", true},       // IPv4 multicast
		{"239.1.2.3", true},       // IPv4 multicast (admin-scoped)
		{"ff02::1", true},         // IPv6 multicast
		{"192.0.2.1", true},       // TEST-NET-1 documentation
		{"198.51.100.1", true},    // TEST-NET-2 documentation
		{"203.0.113.1", true},     // TEST-NET-3 documentation
		{"2001:db8::1", true},     // IPv6 documentation
		// Just outside the widened ranges — must stay public
		{"100.128.0.1", false},     // above CGNAT
		{"192.0.1.1", false},       // above 192.0.0.0/24
		{"198.20.0.1", false},      // above 198.18.0.0/15
		{"223.255.255.255", false}, // last public /8 below the 240/4 reserved block
	}

	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if ip == nil {
				t.Fatalf("failed to parse IP: %s", tt.ip)
			}
			got := isPrivateIP(ip)
			if got != tt.private {
				t.Errorf("isPrivateIP(%s) = %v, want %v", tt.ip, got, tt.private)
			}
		})
	}
}
