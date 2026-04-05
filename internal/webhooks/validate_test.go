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
