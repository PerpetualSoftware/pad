package config

import "testing"

func TestResolvePushGatewaySecret(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"explicit wins", Config{PushGatewaySecret: "p", CloudOutboundSecret: "o", CloudSecret: "a,b"}, "p"},
		{"outbound next", Config{CloudOutboundSecret: "o", CloudSecret: "a,b"}, "o"},
		{"last inbound entry", Config{CloudSecret: "new,old"}, "old"},
		{"blank explicit falls through", Config{PushGatewaySecret: "  ", CloudSecret: "a"}, "a"},
		{"none", Config{}, ""},
	} {
		if got := tc.cfg.ResolvePushGatewaySecret(); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
