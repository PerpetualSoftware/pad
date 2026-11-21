package main

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/config"
	"github.com/PerpetualSoftware/pad/internal/secret"
)

func TestResolvePushGatewaySecret(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.Config
		want string
	}{
		{"explicit wins", config.Config{PushGatewaySecret: secret.New("p"), CloudOutboundSecret: secret.New("o"), CloudSecret: secret.New("a,b")}, "p"},
		{"outbound next", config.Config{CloudOutboundSecret: secret.New("o"), CloudSecret: secret.New("a,b")}, "o"},
		{"last inbound entry", config.Config{CloudSecret: secret.New("new,old")}, "old"},
		{"blank explicit falls through", config.Config{PushGatewaySecret: secret.New("  "), CloudSecret: secret.New("a")}, "a"},
		{"none", config.Config{}, ""},
	} {
		if got := resolvePushGatewaySecret(&tc.cfg); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
