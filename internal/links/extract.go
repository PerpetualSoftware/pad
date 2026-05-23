package links

import (
	"regexp"
	"strings"
)

// WikiLinkKind discriminates the five [[...]] forms the renderer
// supports (see web/src/lib/utils/markdown.ts::renderMarkdown). Phase 1
// of PLAN-1593 only extracts WikiLinkKindRef; the title and
// workspace_ref kinds are reserved for Phase 2 and present here so
// downstream consumers can switch on the full vocabulary now and
// the parser can grow into the remaining forms without breaking
// callers.
type WikiLinkKind string

const (
	// WikiLinkKindRef is [[REF-N]] or [[REF-N|Display]] — the
	// dominant modern form. Stable across title renames.
	WikiLinkKindRef WikiLinkKind = "ref"

	// WikiLinkKindTitle is the legacy [[Title]] / [[collection/Title]]
	// form. Title renames must trigger re-resolution. Phase 2.
	WikiLinkKindTitle WikiLinkKind = "title"

	// WikiLinkKindWorkspaceRef is [[workspace::REF]] /
	// [[workspace::REF|Display]] — points across a workspace
	// boundary. Resolution against the foreign workspace happens at
	// query time, not parse time. Phase 2.
	WikiLinkKindWorkspaceRef WikiLinkKind = "workspace_ref"
)

// WikiLinkRef is one extracted [[...]] occurrence. Position is the
// byte offset of the OPENING `[[` in the ORIGINAL content (not the
// code-stripped scratch buffer). The store uses this offset both for
// stable ordering when an item links to the same target multiple
// times AND for the ~80-char snippet the backlinks handler returns.
type WikiLinkRef struct {
	Kind WikiLinkKind

	// WorkspaceSlug is set only for WikiLinkKindWorkspaceRef (Phase 2).
	WorkspaceSlug string

	// Ref is the literal ref string (e.g. "TASK-5"). Set for
	// WikiLinkKindRef and WikiLinkKindWorkspaceRef. Empty for
	// title-kind rows.
	Ref string

	// Title is the literal title text. Set only for
	// WikiLinkKindTitle (Phase 2). Empty for ref/workspace_ref.
	Title string

	// Display is the [[X|Display]] override. Empty if the link
	// didn't carry a pipe segment. Stored verbatim (no escaping)
	// because the renderer is responsible for HTML-escaping at
	// display time — same convention items.title follows.
	Display string

	// Position is the byte offset of the opening `[[` in the
	// source content. Always points into the ORIGINAL content,
	// not the code-stripped buffer the parser used to find
	// outside-code matches.
	Position int
}

// REF_PATTERN matches a Pad item ref like TASK-5 or BUG-585. Mirrors
// the renderer's REF_PATTERN constant in web/src/lib/utils/markdown.ts
// (we deliberately keep the same shape so server-side and client-side
// extraction agree on what "a ref" looks like).
var refPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]*-\d+$`)

// wikiLinkPattern matches `[[...]]` non-greedily. We split the body
// ourselves to handle the `|Display` segment and the cross-workspace
// `workspace::REF` form rather than baking those into the regex —
// the body-shape logic is easier to read in plain Go than as nested
// capture groups, and the parser already needs the position info the
// regex returns.
//
// `[^\]]+` excludes `]` from the body so a malformed `[[A]]B]]` won't
// gobble characters across the inner closer. Matches the renderer's
// pattern exactly.
var wikiLinkPattern = regexp.MustCompile(`\[\[([^\]]+)\]\]`)

// fencedCodeRanges returns half-open `[start, end)` byte ranges that
// cover every fenced (triple-backtick) code block in `content`,
// including the opening and closing fences themselves. We walk
// the string rather than relying on a regex because:
//
//  1. A bare `regexp.FindAllStringIndex` of ```...``` mis-counts
//     content containing `````` (four+ backticks) — markdown lets
//     the fence length vary.
//  2. We need to distinguish opener vs closer to handle the case
//     where an unclosed fence runs to EOF (a real edge case in
//     drafts the user hasn't finished typing).
//
// Behavior matches the markdown spec: a fence is `\n` + “ ``` “ +
// optional language tag + `\n`, and the matching closer is `\n` +
// the same number of backticks. We're permissive about the leading
// newline at file start (no preceding `\n` required) for the
// content-starts-with-fence case.
func fencedCodeRanges(content string) [][2]int {
	var ranges [][2]int
	i := 0
	n := len(content)
	for i < n {
		// Find the next run of 3+ backticks at a line boundary
		// (either start-of-content or after a newline).
		lineStart := i
		if lineStart > 0 && content[lineStart-1] != '\n' {
			// Advance to next newline; fences must start a line.
			nl := strings.IndexByte(content[i:], '\n')
			if nl < 0 {
				return ranges
			}
			i += nl + 1
			continue
		}
		// At a line start. Count backticks.
		tickStart := i
		for i < n && content[i] == '`' {
			i++
		}
		tickCount := i - tickStart
		if tickCount < 3 {
			// Not a fence opener; skip to next newline.
			nl := strings.IndexByte(content[i:], '\n')
			if nl < 0 {
				return ranges
			}
			i += nl + 1
			continue
		}
		// Found an opener. Find the closing fence: a line that
		// starts with at least `tickCount` backticks. If no closer
		// exists, the fence runs to EOF (covers the rest of the
		// content).
		closer := findFenceCloser(content, i, tickCount)
		if closer < 0 {
			ranges = append(ranges, [2]int{tickStart, n})
			return ranges
		}
		// closer points at the start of the closing tick run;
		// advance past it (and any extra backticks) to find the
		// end of the block.
		j := closer
		for j < n && content[j] == '`' {
			j++
		}
		ranges = append(ranges, [2]int{tickStart, j})
		i = j
	}
	return ranges
}

// findFenceCloser scans forward from `start` looking for a line that
// begins with at least `tickCount` consecutive backticks. Returns the
// index of the first backtick of the closer, or -1 if none exists.
func findFenceCloser(content string, start, tickCount int) int {
	i := start
	n := len(content)
	for i < n {
		// Skip to next line.
		nl := strings.IndexByte(content[i:], '\n')
		if nl < 0 {
			return -1
		}
		lineStart := i + nl + 1
		if lineStart >= n {
			return -1
		}
		// Count backticks at the start of this line.
		j := lineStart
		for j < n && content[j] == '`' {
			j++
		}
		if j-lineStart >= tickCount {
			return lineStart
		}
		i = lineStart
	}
	return -1
}

// inlineCodeRanges returns half-open `[start, end)` byte ranges that
// cover every inline-code span (single-backtick) in `content`, skipping
// over the fenced regions caller has already identified.
//
// Inline code spans are delimited by matching backtick runs of the
// same length: `code` and “co`de“ are both valid. We don't fully
// implement that variant (matching run lengths) because for our
// purpose — exclude `[[...]]` inside any `...` region — counting
// every backtick-delimited stretch as code is conservative and
// correct (false positives there mean we DON'T index a wiki-link
// inside oddly-quoted code, which is exactly what we want).
//
// Span doesn't cross newlines: an unclosed backtick at end-of-line
// is treated as literal text, not the start of a multi-line span.
// Matches CommonMark behavior closely enough for our use.
func inlineCodeRanges(content string, fenced [][2]int) [][2]int {
	var ranges [][2]int
	i := 0
	n := len(content)
	fi := 0 // cursor into fenced ranges
	for i < n {
		// Skip ahead past any fenced range that contains or
		// precedes our cursor.
		for fi < len(fenced) && fenced[fi][1] <= i {
			fi++
		}
		if fi < len(fenced) && fenced[fi][0] <= i {
			i = fenced[fi][1]
			fi++
			continue
		}
		// Look for the next backtick.
		b := strings.IndexByte(content[i:], '`')
		if b < 0 {
			return ranges
		}
		openStart := i + b
		// If the backtick is inside a fenced range, skip past it.
		if fi < len(fenced) && fenced[fi][0] <= openStart && openStart < fenced[fi][1] {
			i = fenced[fi][1]
			fi++
			continue
		}
		// Find the closing backtick on the same line. The opener
		// itself might be a run of backticks (rare), but for our
		// permissive treatment we close on the next backtick we
		// see — see function comment.
		j := openStart + 1
		for j < n && content[j] == '`' {
			j++
		}
		// Scan for the closer.
		closerStart := -1
		for k := j; k < n; k++ {
			if content[k] == '\n' {
				break
			}
			if content[k] == '`' {
				closerStart = k
				break
			}
		}
		if closerStart < 0 {
			// Unclosed backtick — treat as literal, skip past it.
			i = openStart + 1
			continue
		}
		// Advance closer past the run.
		k := closerStart
		for k < n && content[k] == '`' {
			k++
		}
		ranges = append(ranges, [2]int{openStart, k})
		i = k
	}
	return ranges
}

// isInRanges returns true if `pos` falls inside any half-open
// `[start, end)` interval in `ranges`. `ranges` must be sorted by
// start (which both fencedCodeRanges and inlineCodeRanges produce
// naturally by their forward scan). O(log N) binary search would
// be tighter but ranges per item are bounded enough (most bodies
// have under 20 code spans) that linear scan is fine and easier
// to audit.
func isInRanges(pos int, ranges [][2]int) bool {
	for _, r := range ranges {
		if pos < r[0] {
			return false
		}
		if pos < r[1] {
			return true
		}
	}
	return false
}

// ExtractWikiLinks scans `content` for [[...]] occurrences OUTSIDE
// any fenced or inline code region, parses each into a WikiLinkRef,
// and returns them in source order.
//
// Phase 1 of PLAN-1593: only ref-form links (`[[REF-N]]` /
// `[[REF-N|Display]]`) populate the returned slice. Title-form and
// workspace_ref-form links are recognized at parse time (so the
// caller can persist them as `target_kind` placeholders in Phase 2)
// but are NOT emitted today — Phase 2 will flip that switch.
//
// Returns an empty slice on empty input. Never returns an error —
// any bracket sequence that fails to parse is silently skipped
// (the renderer's fallback behavior is the same: unresolved
// `[[X]]` renders as a broken link in the body, not an error).
func ExtractWikiLinks(content string) []WikiLinkRef {
	if content == "" {
		return nil
	}
	fenced := fencedCodeRanges(content)
	inline := inlineCodeRanges(content, fenced)

	var out []WikiLinkRef
	matches := wikiLinkPattern.FindAllStringSubmatchIndex(content, -1)
	for _, m := range matches {
		// m[0]=start of [[, m[1]=end of ]], m[2]=start of body, m[3]=end of body
		linkStart := m[0]
		if isInRanges(linkStart, fenced) || isInRanges(linkStart, inline) {
			continue
		}
		body := content[m[2]:m[3]]
		ref := parseBody(body)
		if ref == nil {
			continue
		}
		ref.Position = linkStart
		// Phase 1: only emit ref-form. Drop title and workspace_ref
		// rows so the Phase-1 store sees only what Phase 1 promises
		// to index. Phase 2 will remove this gate.
		if ref.Kind != WikiLinkKindRef {
			continue
		}
		out = append(out, *ref)
	}
	return out
}

// parseBody decodes the inside of a `[[...]]`. Returns nil if the
// body doesn't match any of the five recognized forms. Mirrors the
// renderer's body-parsing logic in markdown.ts so server-side and
// client-side extraction stay in lockstep.
func parseBody(body string) *WikiLinkRef {
	// Strip a `|Display` suffix first. We split on the FIRST `|`
	// because the display text itself may legally contain pipes
	// (e.g. "[[TASK-5|A | B]]") even though that's rare.
	var display string
	if pipe := strings.IndexByte(body, '|'); pipe >= 0 {
		display = strings.TrimSpace(body[pipe+1:])
		body = body[:pipe]
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}

	// Cross-workspace form: `workspace-slug::REF`. The `::` separator
	// is unambiguous; if it's present, the workspace + ref must each
	// match their patterns or the whole thing falls back to title.
	if sep := strings.Index(body, "::"); sep >= 0 {
		ws := strings.TrimSpace(body[:sep])
		rest := strings.TrimSpace(body[sep+2:])
		if isWorkspaceSlug(ws) && refPattern.MatchString(rest) {
			return &WikiLinkRef{
				Kind:          WikiLinkKindWorkspaceRef,
				WorkspaceSlug: ws,
				Ref:           rest,
				Display:       display,
			}
		}
		// Fall through to title — the renderer's fallback policy.
	}

	// Ref form: a bare REF-N pattern.
	if refPattern.MatchString(body) {
		return &WikiLinkRef{
			Kind:    WikiLinkKindRef,
			Ref:     body,
			Display: display,
		}
	}

	// Legacy collection-qualified title: `collection/Title`. We
	// treat the whole body as the title for storage; the resolver
	// in Phase 2 will split on `/` to bias the lookup.
	// Plain legacy title.
	return &WikiLinkRef{
		Kind:    WikiLinkKindTitle,
		Title:   body,
		Display: display,
	}
}

// workspaceSlugPattern is a conservative subset that mirrors the
// renderer's WORKSPACE_SLUG_PATTERN. Letter/digit-led, hyphen-allowed,
// no trailing hyphen.
var workspaceSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$|^[a-z0-9]$`)

func isWorkspaceSlug(s string) bool {
	return workspaceSlugPattern.MatchString(s)
}
