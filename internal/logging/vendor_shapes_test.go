package logging

import (
	"strings"
	"testing"
)

func vendorBody(n int) string {
	const alphabet = "Ab3_x-Z9q"
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(alphabet)
	}
	return b.String()[:n]
}

func alnum(n int) string {
	return strings.Repeat("aB3", n/3+1)[:n]
}

// vendorTokens are credentials whose shape alone identifies them: a vendor
// prefix plus a body of the documented length. Each must be masked whole
// wherever it appears in text.
func vendorTokens() map[string]string {
	return map[string]string{
		"anthropic api":        "sk-ant-api03-" + vendorBody(95),
		"anthropic admin":      "sk-ant-admin01-" + vendorBody(93),
		"openai project":       "sk-proj-" + vendorBody(120),
		"openai service":       "sk-svcacct-" + vendorBody(120),
		"openai admin":         "sk-admin-" + vendorBody(60),
		"openai legacy":        "sk-" + alnum(48),
		"github fine-grained":  "github_pat_" + alnum(22) + "_" + alnum(59),
		"slack bot":            "xoxb-123456789012-1234567890123-" + alnum(24),
		"slack user":           "xoxp-123456789012-1234567890123-" + alnum(32),
		"aws access key":       "AKIAIOSFODNN7EXAMPLE",
		"aws session key":      "ASIAIOSFODNN7EXAMPLE",
		"google api":           "AIzaSy" + vendorBody(33),
		"gitlab pat":           "glpat-" + vendorBody(20),
		"gitlab pat routable":  "glpat-" + vendorBody(27) + ".01.xyz",
		"oauth access token":   "ory_at_" + vendorBody(43) + "." + vendorBody(43),
		"oauth refresh token":  "ory_rt_" + vendorBody(43) + "." + vendorBody(43),
		"oauth authorize code": "ory_ac_" + vendorBody(43) + "." + vendorBody(43),
	}
}

func TestRedactTextVendorShapes(t *testing.T) {
	for name, tok := range vendorTokens() {
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct{ in, want string }{
				{"key " + tok + " bad", "key REDACTED bad"},
				{tok, "REDACTED"},
				{`"` + tok + `"`, `"REDACTED"`},
				{"sent " + tok + ". Retrying", "sent REDACTED. Retrying"},
			} {
				if got := RedactText(tc.in); got != tc.want {
					t.Errorf("RedactText(%q)\n got %q\nwant %q", tc.in, got, tc.want)
				}
			}
		})
	}
}

// Near misses: a prefix alone, a short body, the prefix inside a longer word,
// the wrong case. Prose and identifiers like these must pass through.
func TestRedactTextVendorShapeNegatives(t *testing.T) {
	for _, in := range []string{
		"the sk-ant- prefix",
		"sk-ant-api03-short",
		"task-ant-api03-" + vendorBody(95),
		"sk-ant-xyz03-" + vendorBody(95),
		"run sk-proj-setup first",
		"ask-proj-" + vendorBody(40),
		"sk-proj-" + vendorBody(39),
		"sk-svcacct-short",
		"ask-svcacct-" + vendorBody(40),
		"open sk-admin-panel",
		"desk-admin-" + vendorBody(40),
		"sk-" + alnum(47),
		"sk-" + alnum(49),
		"ask-" + alnum(48),
		"sk-learn and sk-image are libraries",
		"github_pat_ prefix",
		"github_pat_example",
		"my_github_pat_" + alnum(22) + "_" + alnum(59),
		"GITHUB_PAT_" + alnum(22) + "_" + alnum(59),
		"xoxb-token",
		"xoxb-123",
		"xoxo and hugs",
		"xoxa-123456789012-1234567890123-" + alnum(24),
		"AKIAEXAMPLE",
		"akiaiosfodnn7example",
		"XAKIAIOSFODNN7EXAMPLE",
		"AKIAIOSFODNN7EXAMPLEX",
		"ASIAEXAMPLE",
		"XASIAIOSFODNN7EXAMPLE",
		"ASIAIOSFODNN7EXAMPLEX",
		"AIza",
		"AIzaSyShort",
		"Aizawl is a city",
		"glpat-",
		"glpat-short",
		"ory_at_",
		"ory_at_short.x",
		"theory_at_" + vendorBody(43) + "." + vendorBody(43),
		"ory_xx_" + vendorBody(43) + "." + vendorBody(43),
	} {
		if got := RedactText(in); got != in {
			t.Errorf("RedactText changed %q to %q", in, got)
		}
	}
}

// A longer real key is masked whole, not cut at the minimum length.
func TestRedactTextVendorShapeMasksWholeRun(t *testing.T) {
	tok := "sk-ant-api03-" + vendorBody(200)
	if got := RedactText("x " + tok + " y"); got != "x REDACTED y" {
		t.Errorf("got %q", got)
	}
	gl := "glpat-" + vendorBody(27) + ".01.xyz"
	if got := RedactText("token " + gl + ". Next"); got != "token REDACTED. Next" {
		t.Errorf("got %q", got)
	}
}
