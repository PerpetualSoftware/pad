package links

import (
	"regexp"
	"sort"
	"strings"
)

// ReplaceTitle replaces all [[oldTitle]] with [[newTitle]] in content.
// LEGACY helper used by the document-rename path; case-sensitive,
// no-pipe forms only. For item rename use RewriteWikiTitle below,
// which also handles `[[Title|alias]]`, `[[<slug>/Title]]`, and
// `[[<slug>/Title|alias]]` and matches case-insensitively to mirror
// the renderer's title resolution.
func ReplaceTitle(content, oldTitle, newTitle string) string {
	old := "[[" + oldTitle + "]]"
	new := "[[" + newTitle + "]]"
	return replaceAll(content, old, new)
}

// RewriteWikiTitle rewrites the four title-form wiki-link shapes that
// resolve to an item titled `oldTitle` in collection `collSlug`,
// substituting `newTitle` for the title portion and preserving any
// optional display alias verbatim:
//
//	[[Old Title]]                  → [[New Title]]
//	[[Old Title|alias]]            → [[New Title|alias]]
//	[[<slug>/Old Title]]           → [[<slug>/New Title]]
//	[[<slug>/Old Title|alias]]     → [[<slug>/New Title|alias]]
//
// The title segment matches case-insensitively because resolveTitleTx
// also resolves titles case-insensitively — a source body that wrote
// `[[old title]]` and resolved to "Old Title" via LOWER() comparison
// must get rewritten by this function or the cascade leaves it broken
// (Codex review of TASK-1595 round 1).
//
// Returns content unchanged if `oldTitle` is empty or equal to
// `newTitle`. Caller is responsible for invoking once per rename;
// repeat application is safe but does no useful work.
//
// Known limitation: titles containing the wiki-link escape characters
// (`]`, `|`, `\`) get stored in source content as `\]`, `\|`, `\\`,
// which the editor's grammar at web/src/lib/utils/markdown.ts:461
// supports. This rewriter does NOT attempt escape-aware matching on
// the TITLE segment — a title literally containing `]` would be
// stored escaped and would fail to match the regex's `oldTitle`
// literal. The same limitation exists in the legacy ReplaceTitle
// helper above and the document-rename path; items with such titles
// are vanishingly rare in practice (an item titled `My [Plan]`
// would be a stretch). Promotable to a separate task if a real user
// hits it.
func RewriteWikiTitle(content, oldTitle, newTitle, collSlug string) string {
	if oldTitle == "" || oldTitle == newTitle {
		return content
	}
	// Build per-rename regex. The capturing groups:
	//   1: optional `<slug>/` prefix (or empty)
	//   2: the title segment (matched case-insensitively)
	//   3: optional `|display` suffix including the pipe (or empty)
	//
	// The display segment uses the same `(?:\\.|[^\]\\])*` grammar as
	// the editor (markdown.ts:461) so an alias with escaped `]`/`|`
	// inside doesn't end the match early. Inline `(?i:...)` scopes
	// the case-insensitivity to the title segment only — the slug
	// portion compares against c.slug which is canonically lowercase,
	// and we don't want to accidentally fold case on the optional
	// display either.
	escSlug := regexp.QuoteMeta(collSlug)
	escTitle := regexp.QuoteMeta(oldTitle)
	pattern := `\[\[((?:` + escSlug + `/)?)(` + `(?i:` + escTitle + `))((?:\|(?:\\.|[^\]\\])*)?)\]\]`
	re := regexp.MustCompile(pattern)
	return re.ReplaceAllStringFunc(content, func(match string) string {
		groups := re.FindStringSubmatch(match)
		if len(groups) != 4 {
			return match // defensive — shouldn't happen given the literal pattern
		}
		return "[[" + groups[1] + newTitle + groups[3] + "]]"
	})
}

// RewriteBracketAt rewrites the wiki-link bracket starting at byte
// position `position` in `content`, replacing the bracket's title
// segment with `newTitle`. Used by the rename cascade to rewrite
// only the specific bracket whose item_wiki_links row resolves to
// the renamed item — avoiding the broad-regex hazard from Codex
// round 7 finding 2, where an unrelated `[[OldTitle|alias]]`
// pointing at a literal-pipe-titled item B was corrupted when
// item A "Old Title" was renamed.
//
// The bracket's optional `<collSlug>/` prefix and `|<display>`
// suffix are preserved verbatim. The function does a defensive
// title-segment check so a position whose bracket no longer
// matches the expected target_title (e.g. due to a prior edit
// that shifted offsets without our index catching up) leaves the
// content unchanged — the caller's replaceWikiLinks re-parse will
// reconcile the index regardless.
//
// Matching cases:
//
//   - Bracket body equals targetTitle (case-insensitive) — replace
//     the whole body with `<prefix>newTitle`.
//   - Bracket body starts with `targetTitle + "|"` — replace just
//     the title segment, preserve the `|display` suffix verbatim.
//   - Bracket body equals `<collSlug>/<targetTitle>` — replace the
//     trailing title segment, preserve the slug prefix.
//   - Same with `|display` suffix.
//
// Otherwise the content is returned unchanged.
//
// Like the legacy ReplaceTitle helper, this does NOT distinguish
// code regions from prose. A bracket inside fenced code at the
// recorded position WILL be rewritten — matches the document
// rename path's behavior.
//
// Implemented as the one-element case of RewriteBracketsAt so the two cannot
// drift: the per-bracket decision lives in exactly one place (bracketRewriteAt).
// Behaviour across that refactor is pinned by
// TestRewriteBracketAt_MatchesPreRefactorImplementation and its randomised
// sibling, which compare against the pre-refactor code frozen as an oracle —
// comparing this function to RewriteBracketsAt-of-one would be vacuous, since
// that is now its definition. BUG-2804.
func RewriteBracketAt(content string, position int, targetTitle, newTitle, collSlug string) string {
	out, _ := RewriteBracketsAt(content, []BracketRewrite{{Position: position, TargetTitle: targetTitle}}, newTitle, collSlug)
	return out
}

// BracketRewrite names one bracket to rewrite: the byte offset its `[[` starts
// at, and the target_title the index recorded for it. NewTitle and collSlug are
// shared across a whole call because a rename cascade renames ONE item to ONE
// new title.
type BracketRewrite struct {
	Position    int
	TargetTitle string
}

// RewriteBracketsAt rewrites every bracket named in `rewrites` in a SINGLE pass
// over `content`, returning the new content and how many rewrites it applied.
//
// This exists because calling RewriteBracketAt once per bracket is quadratic in
// the body size, and that is reachable in one request: the cheapest wiki-link is
// five bytes (`[[A]]`) and nothing caps links per item, so a C-byte body carries
// C/5 brackets and each call rebuilt the whole body. Measured before the fix at
// 1.00x the body allocated per call — see internal/store's BUG-2804 probes,
// which measured the cascade at 2.01x body per bracket and showed allocation
// growing 3.83/3.92/3.95 per doubling of C, i.e. O(C^2).
//
// Semantics, which are deliberately stricter than "apply them in some order":
//
//   - `rewrites` may arrive in ANY order. The cascade's SELECT returns positions
//     DESCENDING (so that repeated whole-body rewrites did not invalidate each
//     other's offsets); a single pass needs them ascending, so this function
//     sorts a COPY and never mutates the caller's slice.
//   - A rewrite whose bracket does not match the expected shape is SKIPPED and
//     its bytes are carried verbatim — the same conservative posture
//     RewriteBracketAt has always had, where a drifted offset leaves content
//     alone and the caller's re-parse reconciles the index.
//   - A rewrite that OVERLAPS one already applied is skipped. Distinct brackets
//     cannot overlap, so this only fires on a corrupt or duplicated offset; in
//     that case carrying the bytes verbatim is the only non-corrupting choice.
//   - Applying nothing returns the input string unchanged, allocating nothing.
//     Callers use the count to decide whether a write is owed, which also spares
//     them a full-body string comparison to detect it.
func RewriteBracketsAt(content string, rewrites []BracketRewrite, newTitle, collSlug string) (string, int) {
	if len(rewrites) == 0 {
		return content, 0
	}

	ordered := make([]BracketRewrite, len(rewrites))
	copy(ordered, rewrites)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Position < ordered[j].Position })

	var b strings.Builder
	cursor := 0
	applied := 0
	for _, rw := range ordered {
		if rw.Position < cursor {
			// Overlaps a rewrite already applied (or a duplicate offset).
			continue
		}
		qualified, displaySuffix, bracketEnd, ok := bracketRewriteAt(content, rw.Position, rw.TargetTitle, newTitle, collSlug)
		if !ok {
			continue
		}
		// A rewrite that reproduces the bracket byte-for-byte is NOT a change.
		// Skipping it leaves the identical bytes to the verbatim carry, so the
		// output is the same either way — but `applied` then counts CHANGES
		// rather than matches, and the cascade uses that count to decide
		// whether to write the row at all. Counting matches made a no-op
		// rename rewrite content, bump seq, and emit events for every linker
		// (codex R1 P2). The old code compared whole bodies to decide this;
		// comparing the bracket is the same question asked locally.
		if bracketUnchanged(content, rw.Position, bracketEnd, qualified, newTitle, collSlug, displaySuffix) {
			continue
		}
		if applied == 0 {
			// First real rewrite — size the buffer once. len(content) is the
			// right order of magnitude in both directions (a rename that grows
			// the body reallocates once; one that shrinks it wastes a little).
			b.Grow(len(content))
		}
		b.WriteString(content[cursor:rw.Position])
		b.WriteString("[[")
		if qualified {
			b.WriteString(collSlug)
			b.WriteString("/")
		}
		b.WriteString(newTitle)
		b.WriteString(displaySuffix)
		b.WriteString("]]")
		cursor = bracketEnd
		applied++
	}
	if applied == 0 {
		return content, 0
	}
	b.WriteString(content[cursor:])
	return b.String(), applied
}

// ProjectRewrittenLen returns the byte length RewriteBracketsAt WOULD produce
// for the same arguments, and how many rewrites it would apply, without
// building the result.
//
// It exists so a caller can bound what a cascade will hold BEFORE allocating
// it — refusing after the amplified string is built bounds nothing. Exact
// rather than an estimate: it runs the same per-bracket decision
// (bracketRewriteAt) and the same ordering and skip rules as the real pass, so
// the two cannot disagree about which brackets apply.
//
// Cost is O(total bracket text), not O(len(content) * len(rewrites)) — each
// bracket is inspected once and the untouched spans are only measured.
func ProjectRewrittenLen(content string, rewrites []BracketRewrite, newTitle, collSlug string) (length, applied int) {
	if len(rewrites) == 0 {
		return len(content), 0
	}
	ordered := make([]BracketRewrite, len(rewrites))
	copy(ordered, rewrites)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Position < ordered[j].Position })

	length = len(content)
	cursor := 0
	for _, rw := range ordered {
		if rw.Position < cursor {
			continue
		}
		qualified, displaySuffix, bracketEnd, ok := bracketRewriteAt(content, rw.Position, rw.TargetTitle, newTitle, collSlug)
		if !ok {
			continue
		}
		if bracketUnchanged(content, rw.Position, bracketEnd, qualified, newTitle, collSlug, displaySuffix) {
			// Deliberately does NOT advance the cursor, mirroring the real pass
			// exactly. An earlier version advanced it here, which made the two
			// loops disagree about which overlapping rewrites the guard skips:
			// on `[[A[[B]]]]` with a no-op at 0 and a change at 3, projection
			// reported (10 bytes, 0 applied) while the pass produced 13 bytes
			// and applied 1 — so the cascade charged a read it never charged
			// the rewrite for, and the bound leaked on exactly the corrupt or
			// duplicated offsets the defensive paths exist to handle
			// (codex R2).
			//
			// The pass is the side that must not move: its behaviour is pinned
			// to the descending fold the cascade used to perform. So the
			// projection follows it, never the reverse.
			continue
		}
		// Length arithmetic only — NOTHING is built here. Concatenating a
		// replacement per bracket to measure it would reintroduce, one layer
		// down, exactly the allocate-then-refuse shape the cap exists to
		// prevent (codex R1 P1).
		length += (2 + bracketSegmentLen(qualified, newTitle, collSlug) + len(displaySuffix) + 2) - (bracketEnd - rw.Position)
		cursor = bracketEnd
		applied++
	}
	if applied == 0 {
		return len(content), 0
	}
	return length, applied
}

// bracketSegmentLen is the byte length of the title segment a matched bracket
// will carry: the optional `<collSlug>/` prefix plus newTitle. Computed, never
// built.
func bracketSegmentLen(qualified bool, newTitle, collSlug string) int {
	if qualified {
		return len(collSlug) + 1 + len(newTitle)
	}
	return len(newTitle)
}

// bracketUnchanged reports whether rewriting the bracket at [position,
// bracketEnd) would reproduce it byte-for-byte.
//
// Allocation-free, and now segment-wise: it walks the existing body against the
// optional slug prefix, then newTitle, then the display suffix, rather than
// concatenating them into a candidate string. Building the candidate just to
// compare it would reintroduce the newTitle-proportional allocation that
// bracketRewriteAt exists to avoid — on every bracket, in the projection path,
// before the cap fires (codex R5).
func bracketUnchanged(content string, position, bracketEnd int, qualified bool, newTitle, collSlug, displaySuffix string) bool {
	body := content[position+2 : bracketEnd-2]
	if len(body) != bracketSegmentLen(qualified, newTitle, collSlug)+len(displaySuffix) {
		return false
	}
	if qualified {
		if len(body) < len(collSlug)+1 {
			return false
		}
		if body[:len(collSlug)] != collSlug || body[len(collSlug)] != '/' {
			return false
		}
		body = body[len(collSlug)+1:]
	}
	if len(body) < len(newTitle) || body[:len(newTitle)] != newTitle {
		return false
	}
	return body[len(newTitle):] == displaySuffix
}

// bracketRewriteAt is the per-bracket decision, and the ONLY copy of it. It
// returns the replacement text for the bracket at `position` (including its
// enclosing `[[`/`]]`), the offset just past the bracket it replaces, and
// whether the bracket matched at all.
//
// It reads only content[position:bracketEnd] — that locality is what makes a
// single pass possible, since the rest of the body is pure carry.
func bracketRewriteAt(content string, position int, targetTitle, newTitle, collSlug string) (qualified bool, displaySuffix string, bracketEnd int, ok bool) {
	if position < 0 || position+2 > len(content) {
		return false, "", 0, false
	}
	if content[position:position+2] != "[[" {
		return false, "", 0, false
	}
	rest := content[position+2:]
	closeIdx := strings.Index(rest, "]]")
	if closeIdx < 0 {
		return false, "", 0, false
	}
	body := rest[:closeIdx]
	bracketEnd = position + 2 + closeIdx + 2 // past `]]`

	tLower := strings.ToLower(body)
	ttLower := strings.ToLower(targetTitle)

	// MATCH FIRST, then describe the replacement. Nothing proportional to
	// newTitle is built here at all — this returns a DESCRIPTION (does the
	// qualified prefix apply, what display suffix carries over) and lets each
	// caller either measure it or write it.
	//
	// Both properties matter and both were violated before (codex R1, then R5
	// one layer down). An earlier version composed `collSlug + "/" + newTitle`
	// BEFORE the match check, so every bracket paid for it including the
	// leave-alone exit — and ProjectRewrittenLen calls this once per rewrite
	// BEFORE the cap fires. BUG-2831 then measured that newTitle has no
	// validation bound, so a bracket-dense body with a multi-megabyte new title
	// projected gigabytes of immediately-discarded allocation ahead of the
	// refusal that exists to prevent exactly that.
	//
	// Case 1: body equals target_title (case-insensitive) — no display segment.
	// Case 2: body starts with target_title + "|" — the display segment from
	// the pipe onward carries over verbatim (a SLICE of body, not a copy).
	switch {
	case tLower == ttLower:
	case len(tLower) > len(ttLower) && tLower[len(ttLower)] == '|' && tLower[:len(ttLower)] == ttLower:
		// Spelled out rather than HasPrefix(tLower, ttLower+"|") so the
		// concatenation disappears; it is the same predicate, byte for byte,
		// on the same two already-lowered strings.
		displaySuffix = body[len(targetTitle):] // includes the pipe
	default:
		// Bracket doesn't match the expected shape — leave it alone.
		// (Index drift or a stored full-body title with embedded pipe;
		// the trailing replaceWikiLinks call will reconcile.)
		return false, "", 0, false
	}

	// Only now, on a confirmed match: does the `<collSlug>/` prefix carry over?
	// A qualified body like `[[tasks/Old Title]]` becomes `[[tasks/New Title]]`
	// — the cascade SELECT already proved this row points at the renamed item
	// via stage-2 qualified-fallback resolution, so the slug is guaranteed to
	// match collSlug. Reported as a flag; the caller writes collSlug and "/"
	// directly rather than receiving them spliced onto newTitle.
	if collSlug != "" {
		pfx := collSlug + "/"
		if strings.HasPrefix(strings.ToLower(targetTitle), strings.ToLower(pfx)) {
			qualified = true
		}
	}
	return qualified, displaySuffix, bracketEnd, true
}

// replaceAll substitutes every occurrence of old with new, scanning the INPUT
// once rather than re-scanning its own output.
//
// The previous implementation looped `find old in result; splice new in` until
// no match remained — re-searching the string it was building, including the
// text it had just inserted. When `new` CONTAINS `old` that never terminates
// and the string grows without bound.
//
// Reachable from a user-supplied document title, and measured rather than
// argued: ReplaceTitle("x [[A]] y", "A", "A]] [[A") builds `[[A]] [[A]]`, which
// still contains `[[A]]`, and a probe against the old implementation ran for 3s
// without terminating before being killed. The caller is inside the rename
// transaction, so the hang holds that transaction open indefinitely on either
// dialect. On POSTGRES it also holds the workspace rename advisory lock
// (BUG-2778), blocking every other rename in that workspace behind it; on
// SQLITE that advisory lock is a no-op and the equivalent damage is the
// database-wide write lock the transaction already holds under BEGIN IMMEDIATE.
// Different mechanism, same outcome for everyone else.
//
// strings.Replace with n = -1 has the semantics that were actually wanted:
// non-overlapping, left-to-right, over the input, so inserted text is never
// re-examined. A title that re-embeds the old token now produces one
// substitution per original occurrence and stops.
//
// Found by Codex round 2 on BUG-2785 while enumerating ways the cascade's retry
// could fail to terminate. Pre-existing — but folded into that fix rather than
// filed, because it is three lines against a server hang, and BUG-2785's retry
// calls this helper again per attempt, which makes it reachable more often than
// before.
func replaceAll(s, old, new string) string {
	return strings.Replace(s, old, new, -1)
}
