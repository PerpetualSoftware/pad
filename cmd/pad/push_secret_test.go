package main

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/config"
)

func TestResolvePushGatewaySecret(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.Config
		want string
	}{
		{"explicit wins", config.Config{PushGatewaySecret: "p", CloudOutboundSecret: "o", CloudSecret: "a,b"}, "p"},
		{"outbound next", config.Config{CloudOutboundSecret: "o", CloudSecret: "a,b"}, "o"},
		{"last inbound entry", config.Config{CloudSecret: "new,old"}, "old"},
		{"blank explicit falls through", config.Config{PushGatewaySecret: "  ", CloudSecret: "a"}, "a"},
		{"none", config.Config{}, ""},
	} {
		if got := resolvePushGatewaySecret(&tc.cfg); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
