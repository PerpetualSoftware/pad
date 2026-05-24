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

	// Display is the [[X|Display]] override. Stored verbatim
	// (no trimming, no escape stripping beyond `\]`/`\|`/`\\`)
	// because the renderer is responsible for HTML-escaping at
	// display time — same convention items.title follows. Pair
	// with HasDisplay to distinguish "no pipe" from "pipe with
	// empty display."
	Display string

	// HasDisplay distinguishes `[[REF]]` (no pipe → HasDisplay=false)
	// from `[[REF|]]` (pipe with empty display → HasDisplay=true,
	// Display==""). The client renderer uses `displayOverride ?? title`
	// (JS nullish coalescing — empty-string IS preserved), so an
	// explicit empty display override renders as an empty link. We
	// preserve that distinction in storage so the backlinks panel can
	// reproduce it. Codex round-12 P3.
	HasDisplay bool

	// Position is the byte offset of the opening `[[` in the
	// source content. Always points into the ORIGINAL content,
	// not the code-stripped buffer the parser used to find
	// outside-code matches.
	Position int
}

// REF_PATTERN matches a Pad item ref like TASK-5 or BUG-585. Mirrors
// the renderer's REF_PATTERN constant in web/src/lib/utils/markdown.ts
// — case-insensitive so `[[task-5]]` parses the same way the renderer
// resolves it. Without this parity the renderer would render a mixed-
// case ref as a clickable link while the index silently dropped it
// (Codex round-1 P2). The Display segment is parsed separately so
// case-only differences in the prefix collapse to the canonical
// uppercase form at storage time — see parseBody.
var refPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*-\d+$`)

// wikiLinkPattern matches `[[...]]` while allowing the body to
// contain escaped chars (`\]`, `\|`, `\\`). Mirrors the editor's
// `wikiLinksToMarkdown` grammar in web/src/lib/utils/markdown.ts:461
// (`(?:\\.|[^\]\\])+`), so any link the editor saves can be indexed
// — even if its display text contains a literal `]` or `|`.
//
// renderMarkdown at markdown.ts:300 uses a simpler regex (`[^\]]+`)
// that REJECTS escaped-bracket bodies, so escaped links don't
// currently render as clickable links in the UI. That's a
// pre-existing inconsistency in the editor pipeline; matching the
// permissive grammar here makes the index forward-compatible with a
// renderer fix without leaving a gap when one lands. Codex rounds
// 4/7/10 P2.
var wikiLinkPattern = regexp.MustCompile(`\[\[((?:\\.|[^\]\\])+)\]\]`)

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
		// Find the next fence opener at a line boundary
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
		// At a line start. CommonMark allows 0-3 leading spaces of
		// indentation before a fence opener (4+ spaces makes it an
		// indented code block, which is a different construct). Skip
		// up to 3 leading spaces but bail if we hit a 4th — the
		// renderer would treat that line as code, not a fence opener,
		// and we'd risk false-positive on a wiki-link inside an
		// indented-code paragraph. Codex round-5 finding #2.
		fenceLineStart := i
		spaces := 0
		for spaces < 4 && fenceLineStart < n && content[fenceLineStart] == ' ' {
			fenceLineStart++
			spaces++
		}
		if spaces >= 4 {
			// Indented code, not a fence. Skip the line.
			nl := strings.IndexByte(content[i:], '\n')
			if nl < 0 {
				return ranges
			}
			i += nl + 1
			continue
		}
		// Determine the fence character. CommonMark / marked accept
		// both backtick (`) and tilde (~) fences. The closer must
		// use the same char as the opener and have a matching
		// minimum run length. Codex round-6 finding #1.
		fenceChar := byte(0)
		if fenceLineStart < n {
			switch content[fenceLineStart] {
			case '`', '~':
				fenceChar = content[fenceLineStart]
			}
		}
		if fenceChar == 0 {
			// Not a fence opener of any kind; skip to next newline.
			nl := strings.IndexByte(content[i:], '\n')
			if nl < 0 {
				return ranges
			}
			i += nl + 1
			continue
		}
		// Count fence chars starting at fenceLineStart.
		tickStart := fenceLineStart
		j := fenceLineStart
		for j < n && content[j] == fenceChar {
			j++
		}
		tickCount := j - tickStart
		if tickCount < 3 {
			// Not a fence opener; skip to next newline.
			nl := strings.IndexByte(content[i:], '\n')
			if nl < 0 {
				return ranges
			}
			i += nl + 1
			continue
		}
		// Backtick fences (but NOT tilde fences) reject an info
		// string containing an unescaped backtick — that would
		// otherwise let `` `not a fence ` `` be misread as a fence
		// opener. CommonMark §4.5. Cheap check: if the rest of the
		// opener line contains a backtick, this isn't a real fence.
		if fenceChar == '`' {
			restEnd := strings.IndexByte(content[j:], '\n')
			restLimit := n
			if restEnd >= 0 {
				restLimit = j + restEnd
			}
			if strings.IndexByte(content[j:restLimit], '`') >= 0 {
				// False opener. Skip the line.
				nl := strings.IndexByte(content[i:], '\n')
				if nl < 0 {
					return ranges
				}
				i += nl + 1
				continue
			}
		}
		i = j
		// Found an opener. Find the closing fence using the same
		// char + minimum run length (allowing matching 0-3 space
		// indent — see findFenceCloser). If no closer exists, the
		// fence runs to EOF (covers the rest of the content).
		closer := findFenceCloser(content, i, tickCount, fenceChar)
		if closer < 0 {
			ranges = append(ranges, [2]int{tickStart, n})
			return ranges
		}
		// closer points at the start of the closing fence-char run;
		// advance past it (and any extra fence chars) to find the
		// end of the block.
		k := closer
		for k < n && content[k] == fenceChar {
			k++
		}
		ranges = append(ranges, [2]int{tickStart, k})
		i = k
	}
	return ranges
}

// findFenceCloser scans forward from `start` looking for a line that
// begins with at least `tickCount` consecutive `fenceChar` characters
// (after up to three leading spaces of optional indentation, matching
// CommonMark fenced-code semantics), with NOTHING but spaces after
// the closing fence run on the same line. Returns the index of the
// first fence char of the closer, or -1 if none exists.
//
// CommonMark §4.5 requires that the closing fence line contain only
// the fence + optional trailing spaces — a line like ```not-closed
// inside a still-open fence is NOT a valid closer. Without that
// strictness, the extractor would prematurely terminate the code
// range and leak later refs inside the still-rendered code block.
// Codex round-6 finding #2.
func findFenceCloser(content string, start, tickCount int, fenceChar byte) int {
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
		// CommonMark allows the closing fence to be indented 0-3
		// spaces, independent of the opener's indentation. 4+ spaces
		// would be an indented-code line, not a closer.
		closerStart := lineStart
		spaces := 0
		for spaces < 4 && closerStart < n && content[closerStart] == ' ' {
			closerStart++
			spaces++
		}
		if spaces >= 4 {
			i = lineStart
			continue
		}
		// Count fence chars at this position.
		j := closerStart
		for j < n && content[j] == fenceChar {
			j++
		}
		if j-closerStart >= tickCount {
			// Strictness: after the fence-char run, the rest of
			// the line must be only spaces (then newline or EOF).
			// Anything else (info string, more chars) disqualifies
			// this as a closer. CommonMark §4.5.
			rest := j
			for rest < n && content[rest] != '\n' {
				if content[rest] != ' ' {
					// Not a valid closer; keep scanning later
					// lines for the real closer.
					break
				}
				rest++
			}
			if rest >= n || content[rest] == '\n' {
				return closerStart
			}
		}
		i = lineStart
	}
	return -1
}

// inlineCodeRanges returns half-open `[start, end)` byte ranges that
// cover every inline-code span in `content`, skipping over the fenced
// regions caller has already identified.
//
// CommonMark §6.1: an inline-code span is opened by a run of N
// consecutive backticks and CLOSED by the next run of EXACTLY N
// consecutive backticks on the same line. Backtick runs of any
// length other than N are part of the code text — they don't close
// the span. This matters for our purpose: a body like
// “ “has ` inside [[X-1]]“ “ is a single span whose code text
// includes a stray single backtick AND the wiki-link, and the link
// must NOT be indexed because the renderer shows it as code.
//
// Without matching run lengths (Codex round-7 finding #2), the
// parser would treat the stray single backtick as a closer, end
// the range early, and false-positive on `[[X-1]]`.
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
		// Count the opener's backtick run.
		j := openStart + 1
		for j < n && content[j] == '`' {
			j++
		}
		openLen := j - openStart
		// Scan for a closing backtick RUN of EXACTLY `openLen`
		// backticks. CommonMark §6.1 allows code spans to cross
		// single newlines BUT a blank line (a line containing only
		// whitespace) ends the enclosing paragraph and therefore
		// terminates the span. We allow single-line wraps but break
		// on blank lines — Codex round-9 P1.
		closerStart := -1
		closerEnd := -1
		k := j
		for k < n {
			if content[k] == '\n' {
				// Look at the next line: if it's blank
				// (only whitespace before the next newline
				// or EOF), the span ends here unmatched.
				if isBlankLineAt(content, k+1, n) {
					break
				}
				k++
				continue
			}
			if content[k] != '`' {
				k++
				continue
			}
			runStart := k
			for k < n && content[k] == '`' {
				k++
			}
			runLen := k - runStart
			if runLen == openLen {
				closerStart = runStart
				closerEnd = k
				break
			}
			// Wrong-length run; consumed by the loop, keep scanning.
		}
		if closerStart < 0 {
			// Unclosed (or only mismatched runs to scope end) —
			// treat opener as literal text and resume one byte
			// past it.
			i = openStart + 1
			continue
		}
		ranges = append(ranges, [2]int{openStart, closerEnd})
		i = closerEnd
	}
	return ranges
}

// isBlankLineAt returns true if the line starting at byte position
// `pos` contains only whitespace (space or tab) before its newline,
// or runs to EOF without any non-whitespace. Per CommonMark a blank
// line is one with no chars or only whitespace; this helper matches
// that definition for the purpose of bounding multi-line inline code
// spans (inline code spans don't cross blank lines).
func isBlankLineAt(content string, pos, n int) bool {
	for i := pos; i < n; i++ {
		c := content[i]
		if c == '\n' {
			return true // line had no non-whitespace chars
		}
		if c != ' ' && c != '\t' {
			return false
		}
	}
	return true // EOF with only whitespace counts as blank
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
// editor's body-parsing logic in markdown.ts so server-side and
// client-side extraction stay in lockstep — including the editor's
// `\]`, `\|`, `\\` escape sequences (Codex round-10 P2).
func parseBody(body string) *WikiLinkRef {
	// Split on the FIRST UNESCAPED `|`. `\|` is part of the key or
	// display text (depending on which side of the split it's on)
	// and must NOT cleave the body. Mirrors splitWikiBody at
	// markdown.ts:664. The display side is preserved verbatim
	// (post-unescape) — the renderer doesn't trim it, and the
	// WikiLinkRef.Display doc comment promises verbatim storage.
	// Trimming would silently differ from client behavior on
	// padded display text like `[[TASK-1|  spaces  ]]`. Codex
	// round-11 P3.
	var display string
	hasDisplay := false
	if key, suffix, ok := splitOnUnescapedPipe(body); ok {
		display = unescapeWikiBody(suffix)
		hasDisplay = true
		body = key
	}
	// The key/ref side still gets trimmed because refPattern is
	// anchored — leading/trailing whitespace would force the whole
	// body to fall through to the title kind even though the
	// renderer would resolve it as a ref. parseBody's job is to
	// recognize the SHAPE; whitespace forgiveness in the key is
	// part of that.
	body = unescapeWikiBody(strings.TrimSpace(body))
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
				HasDisplay:    hasDisplay,
			}
		}
		// Fall through to title — the renderer's fallback policy.
	}

	// Ref form: a bare REF-N pattern. Normalize the prefix to upper-
	// case at this single chokepoint — collection prefixes are
	// canonically uppercase in `collections.prefix`, and the
	// resolver/backlinks queries compare against that column. The
	// renderer accepts mixed case for input convenience; we store
	// the canonical form so the index has one shape per (workspace,
	// prefix, number) and downstream callers don't need to be
	// case-aware (Codex round-1 P2).
	if refPattern.MatchString(body) {
		return &WikiLinkRef{
			Kind:       WikiLinkKindRef,
			Ref:        canonicalizeRef(body),
			Display:    display,
			HasDisplay: hasDisplay,
		}
	}

	// Legacy collection-qualified title: `collection/Title`. We
	// treat the whole body as the title for storage; the resolver
	// in Phase 2 will split on `/` to bias the lookup.
	// Plain legacy title.
	return &WikiLinkRef{
		Kind:       WikiLinkKindTitle,
		Title:      body,
		Display:    display,
		HasDisplay: hasDisplay,
	}
}

// workspaceSlugPattern is a conservative subset that mirrors the
// renderer's WORKSPACE_SLUG_PATTERN. Letter/digit-led, hyphen-allowed,
// no trailing hyphen.
var workspaceSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$|^[a-z0-9]$`)

func isWorkspaceSlug(s string) bool {
	return workspaceSlugPattern.MatchString(s)
}

// splitOnUnescapedPipe scans `body` for the first `|` that isn't
// preceded by an unescaped `\`, splitting the body into (key,
// display, found). Mirrors splitWikiBody at markdown.ts:664. A `\`
// always consumes the following byte (even if it's not a recognized
// escape) so the algorithm can't get desynced by stray backslashes.
func splitOnUnescapedPipe(body string) (key, suffix string, found bool) {
	i := 0
	for i < len(body) {
		if body[i] == '\\' && i+1 < len(body) {
			i += 2
			continue
		}
		if body[i] == '|' {
			return body[:i], body[i+1:], true
		}
		i++
	}
	return body, "", false
}

// unescapeWikiBody undoes the editor's body-escape sequences:
// `\]` → `]`, `\|` → `|`, `\\` → `\`. Other backslash sequences are
// left as-is (the renderer does the same — see unescapeWikiBody at
// markdown.ts:657). Idempotent on already-unescaped strings.
func unescapeWikiBody(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			next := s[i+1]
			if next == '\\' || next == ']' || next == '|' {
				b.WriteByte(next)
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// canonicalizeRef uppercases the prefix portion of a "PREFIX-N" ref
// so the index has a single canonical shape per (workspace, prefix,
// number) regardless of how the author cased the source. The number
// segment is unchanged (it can only contain digits per refPattern).
//
// `[[task-5]]` → "TASK-5". `[[Task-5]]` → "TASK-5". `[[TASK-5]]` →
// "TASK-5" (no-op). Inputs that don't contain a hyphen pass through
// untouched (refPattern would have rejected them anyway; callers
// invariantly hold to "matches refPattern" before calling).
func canonicalizeRef(ref string) string {
	dash := strings.LastIndexByte(ref, '-')
	if dash < 0 {
		return strings.ToUpper(ref)
	}
	return strings.ToUpper(ref[:dash]) + ref[dash:]
}
