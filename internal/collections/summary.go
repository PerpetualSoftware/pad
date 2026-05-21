package collections

// PlaybookSummary extracts a short prose hint from a playbook body. Picks the
// first non-heading non-empty paragraph and caps at ~240 chars so summary
// payloads stay compact.
//
// Lives here (rather than in internal/server/) so the bootstrap handler, the
// library HTTP endpoints, and any future surface that wants a playbook
// summary all share one algorithm. The bootstrap handler previously owned
// this helper (handlers_bootstrap.go) — TASK-1561 hoisted it to the
// collections package alongside the library data it summarizes.
func PlaybookSummary(body string) string {
	const maxLen = 240
	const ellipsis = "…"
	for _, line := range splitLines(body) {
		trimmed := trimLeadingSpaces(line)
		if trimmed == "" {
			continue
		}
		// Skip markdown headings — they're labels, not summaries.
		if len(trimmed) > 0 && trimmed[0] == '#' {
			continue
		}
		if len(trimmed) > maxLen {
			return trimmed[:maxLen-len(ellipsis)] + ellipsis
		}
		return trimmed
	}
	return ""
}

// splitLines is a small dependency-free helper. We avoid bufio.Scanner here
// because the typical body is small (under 50KB) and allocating a scanner
// per playbook is wasteful at this scale.
func splitLines(s string) []string {
	out := []string{}
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func trimLeadingSpaces(s string) string {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return s[i:]
}
