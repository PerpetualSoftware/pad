package main

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/config"
)

// TASK-3321 G4: /.well-known/openai-apps-challenge, built through wireMCP
// over the production SetMCPConfig binding (newCapFixture), so a token that
// never reaches the server fails here.

const g4Token = "openai-challenge-tok_123"

func g4Cloud(token string) config.Config {
	c := cfgCloud
	c.OpenAIAppsChallenge = token
	return c
}

var g4Challenge = capRoute{method: "GET", path: "/.well-known/openai-apps-challenge"}

func TestG4_ChallengeServesExactTokenOnMCPHost(t *testing.T) {
	f := newCapFixture(t, g4Cloud(g4Token), true, nil, "")
	rr := f.doAs(t, g4Challenge, "mcp.getpad.dev", "192.0.2.10:4242")
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body %q, want 200", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); got != g4Token {
		t.Fatalf("body %q, want exactly %q", got, g4Token)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type %q, want text/plain", ct)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control %q, want no-store", cc)
	}
}

// Every refusal is the U0a 404, byte-identical to an unowned well-known path.
func TestG4_ChallengeRefusalsAreTheWellKnown404(t *testing.T) {
	cases := []struct {
		name  string
		cfg   config.Config
		cloud bool
		host  string
	}{
		{"unset", g4Cloud(""), true, "mcp.getpad.dev"},
		{"set but the app host", g4Cloud(g4Token), true, "app.getpad.dev"},
		{"set but an unrelated host", g4Cloud(g4Token), true, "evil.example"},
		{"set but self-host", func() config.Config { c := cfgHTTPS; c.OpenAIAppsChallenge = g4Token; return c }(), false, "pad.example.com"},
		{"token unusable", g4Cloud("two words"), true, "mcp.getpad.dev"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCapFixture(t, tc.cfg, tc.cloud, nil, "true")
			want := f.doAs(t, capRoute{method: "GET", path: "/.well-known/openid-configuration"}, tc.host, "192.0.2.10:4242")
			got := f.doAs(t, g4Challenge, tc.host, "192.0.2.10:4242")
			if want.Code != http.StatusNotFound || got.Code != want.Code || got.Body.String() != want.Body.String() ||
				got.Header().Get("Content-Type") != want.Header().Get("Content-Type") {
				t.Fatalf("challenge = %d %q (%s), want the unowned-path 404 %d %q (%s)",
					got.Code, got.Body.String(), got.Header().Get("Content-Type"),
					want.Code, want.Body.String(), want.Header().Get("Content-Type"))
			}
		})
	}
}

// Setting the token changes no other /.well-known answer.
func TestG4_OtherWellKnownPathsUnchanged(t *testing.T) {
	without := newCapFixture(t, g4Cloud(""), true, nil, "")
	with := newCapFixture(t, g4Cloud(g4Token), true, nil, "")
	for _, p := range []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/mcp",
		"/.well-known/oauth-authorization-server",
		"/.well-known/openid-configuration",
		"/.well-known/openai-apps-challenge/extra",
	} {
		for _, host := range []string{"mcp.getpad.dev", "app.getpad.dev"} {
			a := without.doAs(t, capRoute{method: "GET", path: p}, host, "192.0.2.10:4242")
			b := with.doAs(t, capRoute{method: "GET", path: p}, host, "192.0.2.10:4242")
			if a.Code != b.Code || a.Body.String() != b.Body.String() {
				t.Errorf("%s on %s: %d %q without the token, %d %q with it", p, host, a.Code, a.Body.String(), b.Code, b.Body.String())
			}
			if p == "/.well-known/openai-apps-challenge/extra" && a.Code != http.StatusNotFound {
				t.Errorf("%s on %s answered %d, want 404", p, host, a.Code)
			}
		}
	}
}
