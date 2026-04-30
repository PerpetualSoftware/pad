package email

import (
	"strings"
	"testing"
)

// TestBuildHTMLShellSelfHostedNeutral pins that self-hosted output
// carries no getpad.dev marketing footer. Operators ship Pad under
// their own brand and would not want the hosted-service footer
// imposed on transactional emails sent from their deployment.
func TestBuildHTMLShellSelfHostedNeutral(t *testing.T) {
	out := buildHTMLShell("<p>body</p>", "footer note", false)

	if !strings.Contains(out, "<strong style=\"font-size: 18px;\">Pad</strong>") {
		t.Error("self-hosted should keep the Pad wordmark header")
	}
	if !strings.Contains(out, "<p>body</p>") {
		t.Error("self-hosted should include the body content verbatim")
	}
	if !strings.Contains(out, "footer note") {
		t.Error("self-hosted should include the footer note")
	}

	// The Cloud-only artifacts must not appear in self-hosted output.
	cloudOnly := []string{
		"github.com/PerpetualSoftware/pad",
		"getpad.dev/docs",
		"getpad.dev/changelog",
		"getpad.dev/contribute",
		"getpad.dev/faq",
		"getpad.dev/security",
		"getpad.dev/privacy",
		"getpad.dev/terms",
		"getpad.dev/subprocessors",
		"Perpetual Software",
	}
	for _, marker := range cloudOnly {
		if strings.Contains(out, marker) {
			t.Errorf("self-hosted output unexpectedly contained Cloud-only marker %q", marker)
		}
	}
}

// TestBuildHTMLShellCloudIncludesMarketingFooter pins the Cloud-mode
// marketing-footer link order from docs/brand.md §7 and the copyright
// line. Full canonical order: GitHub → Docs → Changelog → Contribute
// → FAQ → Security → Privacy → Terms → Sub-processors. Tests check
// pairwise substring positions rather than using a regex so a future
// code review can read the assertion at a glance.
func TestBuildHTMLShellCloudIncludesMarketingFooter(t *testing.T) {
	out := buildHTMLShell("<p>body</p>", "footer note", true)

	for _, marker := range []string{
		"github.com/PerpetualSoftware/pad",
		"getpad.dev/docs",
		"getpad.dev/changelog",
		"getpad.dev/contribute",
		"getpad.dev/faq",
		"getpad.dev/security",
		"getpad.dev/privacy",
		"getpad.dev/terms",
		"getpad.dev/subprocessors",
		"Pad &middot; Perpetual Software",
	} {
		if !strings.Contains(out, marker) {
			t.Errorf("Cloud output missing marketing-footer marker %q", marker)
		}
	}

	// Order matters per docs/brand.md §7. Pinning pairwise positions
	// across the full canonical list so a future reorder forces the
	// test author to revisit the brand spec contract intentionally.
	pairs := [][2]string{
		{"github.com/PerpetualSoftware/pad", "getpad.dev/docs"},
		{"getpad.dev/docs", "getpad.dev/changelog"},
		{"getpad.dev/changelog", "getpad.dev/contribute"},
		{"getpad.dev/contribute", "getpad.dev/faq"},
		{"getpad.dev/faq", "getpad.dev/security"},
		{"getpad.dev/security", "getpad.dev/privacy"},
		{"getpad.dev/privacy", "getpad.dev/terms"},
		{"getpad.dev/terms", "getpad.dev/subprocessors"},
	}
	for _, pair := range pairs {
		if strings.Index(out, pair[0]) > strings.Index(out, pair[1]) {
			t.Errorf("Cloud footer link order violated: %q must precede %q", pair[0], pair[1])
		}
	}
}

// TestBuildPlainShellMatchesHTMLBranching ensures the plain-text shell
// follows the same self-hosted vs Cloud branching as the HTML helper.
// Plain text has its own minimal format (no chrome) but must still
// gate the marketing-link block on cloudMode.
func TestBuildPlainShellMatchesHTMLBranching(t *testing.T) {
	selfHosted := buildPlainShell("hello", "received because…", false)
	cloud := buildPlainShell("hello", "received because…", true)

	if !strings.Contains(selfHosted, "received because…") {
		t.Error("self-hosted plain shell should include the footer note")
	}
	if strings.Contains(selfHosted, "github.com/PerpetualSoftware/pad") {
		t.Error("self-hosted plain shell should NOT include marketing links")
	}

	if !strings.Contains(cloud, "github.com/PerpetualSoftware/pad") ||
		!strings.Contains(cloud, "getpad.dev/docs") ||
		!strings.Contains(cloud, "Perpetual Software") {
		t.Error("Cloud plain shell should include the marketing link block + copyright")
	}
}
