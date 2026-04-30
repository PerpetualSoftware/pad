package email

import (
	"fmt"
	"strings"
	"time"
)

// buildHTMLShell wraps body HTML in the standard transactional-email
// chrome — header (Pad wordmark) + body + footer note + (Cloud only)
// marketing footer (copyright line + canonical link list).
//
// Self-hosted output is byte-equivalent to the prior inline templates so
// operators see no visible change after this refactor — they ship Pad
// under their own brand and getpad.dev marketing links would be wrong on
// their notifications.
//
// Visual contract: docs/brand.md §6 (header) + §7 (footer link order).
// Email is light-themed for cross-client readability — emails generally
// don't follow the dark theme of the in-app surfaces. The accent color
// (#2563eb) is kept from the prior templates because it has known
// contrast properties on light backgrounds.
//
// The bodyHTML parameter is interpolated verbatim — callers must
// HTML-escape any user-controlled content before passing it in. The
// footerNote is a single-paragraph "you received this because…"
// disclosure rendered in the small grey footer text directly above
// the optional marketing block.
func buildHTMLShell(bodyHTML, footerNoteHTML string, cloudMode bool) string {
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html>
<html>
<head><meta charset="utf-8"></head>
<body style="font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; color: #1a1a1a; max-width: 560px; margin: 0 auto; padding: 40px 20px;">
  <div style="margin-bottom: 32px;">
    <strong style="font-size: 18px;">Pad</strong>
  </div>
`)
	b.WriteString(bodyHTML)
	b.WriteString(`  <hr style="border: none; border-top: 1px solid #e5e5e5; margin: 32px 0;" />
`)
	if footerNoteHTML != "" {
		b.WriteString(`  <p style="font-size: 12px; color: #999;">
    `)
		b.WriteString(footerNoteHTML)
		b.WriteString(`
  </p>
`)
	}
	if cloudMode {
		// Marketing footer block — full canonical link list from docs/brand.md
		// §7, in the order the brand spec mandates: GitHub, Docs, Changelog,
		// Contribute, FAQ, Security, Privacy, Terms, Sub-processors. The
		// auth-page AuthFooter component carries the same nine links; emails
		// get the same set so the brand spec's "Full parity" promise for
		// transactional mail (§1) is honored. Codex caught a trimmed subset
		// on first review (TASK-907 round 1).
		year := time.Now().UTC().Year()
		b.WriteString(fmt.Sprintf(`  <p style="font-size: 11px; color: #999; line-height: 1.5; margin-top: 16px;">
    <a href="https://github.com/PerpetualSoftware/pad" style="color: #999; text-decoration: underline;">GitHub</a>
    &nbsp;·&nbsp;
    <a href="https://getpad.dev/docs" style="color: #999; text-decoration: underline;">Docs</a>
    &nbsp;·&nbsp;
    <a href="https://getpad.dev/changelog" style="color: #999; text-decoration: underline;">Changelog</a>
    &nbsp;·&nbsp;
    <a href="https://getpad.dev/contribute" style="color: #999; text-decoration: underline;">Contribute</a>
    &nbsp;·&nbsp;
    <a href="https://getpad.dev/faq" style="color: #999; text-decoration: underline;">FAQ</a>
    &nbsp;·&nbsp;
    <a href="https://getpad.dev/security" style="color: #999; text-decoration: underline;">Security</a>
    &nbsp;·&nbsp;
    <a href="https://getpad.dev/privacy" style="color: #999; text-decoration: underline;">Privacy</a>
    &nbsp;·&nbsp;
    <a href="https://getpad.dev/terms" style="color: #999; text-decoration: underline;">Terms</a>
    &nbsp;·&nbsp;
    <a href="https://getpad.dev/subprocessors" style="color: #999; text-decoration: underline;">Sub-processors</a>
  </p>
  <p style="font-size: 11px; color: #aaa; margin-top: 8px;">
    &copy; %d Pad &middot; Perpetual Software
  </p>
`, year))
	}
	b.WriteString(`</body>
</html>`)
	return b.String()
}

// buildPlainShell builds the plain-text body for transactional emails.
// Self-hosted output is byte-equivalent to the prior templates. Cloud
// output adds a small marketing-link block and copyright line below the
// per-template footer note. Plain text intentionally stays minimal —
// no chrome, no decorative separators beyond the dashed rule.
func buildPlainShell(bodyText, footerNoteText string, cloudMode bool) string {
	var b strings.Builder
	b.WriteString(bodyText)
	if footerNoteText != "" {
		b.WriteString("\n\n---\n")
		b.WriteString(footerNoteText)
	}
	if cloudMode {
		// Same nine canonical links as the HTML branch (docs/brand.md §7).
		// Plain text uses fixed-width labels for legibility in monospaced
		// mail clients; URL alignment doesn't matter visually but reads
		// cleanly when piped through a screen reader.
		year := time.Now().UTC().Year()
		b.WriteString(fmt.Sprintf(`

GitHub:         https://github.com/PerpetualSoftware/pad
Docs:           https://getpad.dev/docs
Changelog:      https://getpad.dev/changelog
Contribute:     https://getpad.dev/contribute
FAQ:            https://getpad.dev/faq
Security:       https://getpad.dev/security
Privacy:        https://getpad.dev/privacy
Terms:          https://getpad.dev/terms
Sub-processors: https://getpad.dev/subprocessors

(c) %d Pad — Perpetual Software`, year))
	}
	return b.String()
}
