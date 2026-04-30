package main

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/config"
)

// TestCLIAuthBrowserURLRewritesBindAllHost covers the regression from
// TASK-839: when the local server is bound to a bind-all address such as
// 0.0.0.0 or ::, the CLI must NOT print a login URL containing that
// address. Browsers do not reliably resolve 0.0.0.0/:: as connect
// destinations. cliAuthBrowserURL delegates to cfg.BrowserURL(), which
// is responsible for the rewrite — these cases verify the wiring stays
// correct end-to-end.
func TestCLIAuthBrowserURLRewritesBindAllHost(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
		code string
		want string
	}{
		{
			name: "bind-all IPv4 rewritten to 127.0.0.1",
			cfg:  &config.Config{Host: "0.0.0.0", Port: 7777},
			code: "abc123",
			want: "http://127.0.0.1:7777/auth/cli/abc123",
		},
		{
			name: "bind-all IPv6 rewritten to 127.0.0.1",
			cfg:  &config.Config{Host: "::", Port: 7777},
			code: "abc123",
			want: "http://127.0.0.1:7777/auth/cli/abc123",
		},
		{
			name: "empty host rewritten to 127.0.0.1",
			cfg:  &config.Config{Host: "", Port: 7777},
			code: "abc123",
			want: "http://127.0.0.1:7777/auth/cli/abc123",
		},
		{
			name: "explicit loopback host preserved",
			cfg:  &config.Config{Host: "127.0.0.1", Port: 7777},
			code: "abc123",
			want: "http://127.0.0.1:7777/auth/cli/abc123",
		},
		{
			name: "explicit URL (Remote/Cloud) preserved verbatim",
			cfg:  &config.Config{URL: "https://app.getpad.dev"},
			code: "WDJBMJHT",
			want: "https://app.getpad.dev/auth/cli/WDJBMJHT",
		},
		{
			name: "explicit URL trims trailing slash before joining",
			cfg:  &config.Config{URL: "https://app.getpad.dev/"},
			code: "WDJBMJHT",
			want: "https://app.getpad.dev/auth/cli/WDJBMJHT",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cliAuthBrowserURL(tc.cfg, tc.code)
			if got != tc.want {
				t.Fatalf("cliAuthBrowserURL() = %q, want %q", got, tc.want)
			}
		})
	}
}
