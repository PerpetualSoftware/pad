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
	out, _ := RewriteBracketsAt(content, []BracketRewrite{{Position: position, TargetTitle: targetTitle}},
		NewTitleEscaper(newTitle, collSlug))
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
func RewriteBracketsAt(content string, rewrites []BracketRewrite, esc TitleEscaper) (string, int) {
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
		qualified, displaySuffix, bracketEnd, ok := bracketRewriteAt(content, rw.Position, rw.TargetTitle, esc.collSlugForMatch())
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
		if bracketUnchanged(content, rw.Position, bracketEnd, qualified, esc, displaySuffix) {
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
			b.WriteString(esc.escSlug)
			b.WriteString("/")
		}
		b.WriteString(esc.escTitle)
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
func ProjectRewrittenLen(content string, rewrites []BracketRewrite, esc TitleEscaper) (length, applied int) {
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
		qualified, displaySuffix, bracketEnd, ok := bracketRewriteAt(content, rw.Position, rw.TargetTitle, esc.collSlugForMatch())
		if !ok {
			continue
		}
		if bracketUnchanged(content, rw.Position, bracketEnd, qualified, esc, displaySuffix) {
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
		length += (2 + esc.segmentLen(qualified) + len(displaySuffix) + 2) - (bracketEnd - rw.Position)
		cursor = bracketEnd
		applied++
	}
	if applied == 0 {
		return len(content), 0
	}
	return length, applied
}

// escapeWikiBody escapes a title for emission INSIDE a `[[...]]` body, so the
// parser reads back exactly the title that went in.
//
// Byte-for-byte the same rule as the editor's escapeWikiBody
// (web/src/lib/utils/markdown.ts:748): backslash first, then `]` and `|`.
// Doing backslash first is what keeps it the exact inverse of
// unescapeWikiBody — escaping `]` first would leave the introduced backslashes
// to be escaped again.
//
// BUG-2805: the rewriter previously emitted newTitle by plain concatenation.
// A title containing `]` produced a bracket the grammar cannot parse at all
// (`\[\[((?:\\.|[^\]\\])+)\]\]`), so the reparse found no link and DELETED the
// index row — permanent, unrecoverable damage from an ordinary rename, since a
// later rename has no row left to cascade. A title containing `\]` produced
// valid syntax for a DIFFERENT title.
func escapeWikiBody(s string) string {
	if !strings.ContainsAny(s, `\]|`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\', ']', '|':
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// escapedWikiBodyLen is len(escapeWikiBody(s)) WITHOUT building it.
//
// The projection path runs before the cascade's cap can fire, so it must not
// materialise anything proportional to the title (codex R5, BUG-2804). This is
// that rule applied to the escaped form.
func escapedWikiBodyLen(s string) int {
	n := len(s)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\', ']', '|':
			n++
		}
	}
	return n
}

// TitleEscaper carries a rename's emission-ready forms, computed ONCE per
// cascade rather than per source or per bracket.
//
// It exists for the allocation reason, not for tidiness. A cascade renames one
// item to one new title, but RewriteBracketsAt and ProjectRewrittenLen are
// called once per SOURCE — and the scan bound admits a very large number of
// sources when their titles are short. Escaping inside those functions would
// therefore multiply an unbounded newTitle by an unbounded source count, which
// is the BUG-2804 R5 defect in a new costume.
type TitleEscaper struct {
	escTitle string
	escSlug  string
	// rawSlug is the UNESCAPED slug; matching compares unescaped values.
	rawSlug string
	// Lengths are carried separately so the projection path can measure
	// without touching the strings at all.
	titleLen int
	slugLen  int
}

// NewTitleEscaper precomputes the escaped forms for one rename.
func NewTitleEscaper(newTitle, collSlug string) TitleEscaper {
	return TitleEscaper{
		escTitle: escapeWikiBody(newTitle),
		escSlug:  escapeWikiBody(collSlug),
		rawSlug:  collSlug,
		titleLen: escapedWikiBodyLen(newTitle),
		slugLen:  escapedWikiBodyLen(collSlug),
	}
}

// scanBracketBody finds the body of the wiki-link bracket starting at
// `position`, honouring the escape grammar.
//
// It implements exactly the parser's body production
// (`(?:\\.|[^\]\\])+`, extract.go): a backslash consumes the next byte
// whatever it is, an unescaped `]` is only legal as the first half of the
// closing `]]`, and the body must be non-empty.
//
// The naive `strings.Index(rest, "]]")` this replaces finds the WRONG close on
// an escaped body: in `[[A\]]]` it stops at the `]` belonging to the `\]`
// escape and returns the body `A\`. That was latent while the rewriter never
// emitted escapes; BUG-2805's fix makes escaped bodies routine, so the scan has
// to agree with the parser or each rename corrupts what the last one wrote.
func scanBracketBody(content string, position int) (body string, bracketEnd int, ok bool) {
	start := position + 2
	i := start
	for i < len(content) {
		switch content[i] {
		case '\\':
			if i+1 >= len(content) {
				return "", 0, false // trailing backslash: `\\.` has nothing to consume
			}
			i += 2
		case ']':
			if i+1 < len(content) && content[i+1] == ']' {
				if i == start {
					return "", 0, false // empty body; the production is `+`
				}
				return content[start:i], i + 2, true
			}
			return "", 0, false // bare `]` is not in the body production
		default:
			i++
		}
	}
	return "", 0, false
}

// segmentLen is the byte length of the title segment a matched bracket will
// carry: the optional escaped `<collSlug>/` prefix plus the escaped title.
// Computed from precomputed lengths, never built.
func (e TitleEscaper) segmentLen(qualified bool) int {
	if qualified {
		return e.slugLen + 1 + e.titleLen
	}
	return e.titleLen
}

// collSlugForMatch returns the RAW slug the matcher compares target_title
// against. Matching works on unescaped values, so the escaped form must not
// leak into it.
func (e TitleEscaper) collSlugForMatch() string { return e.rawSlug }

// bracketUnchanged reports whether rewriting the bracket at [position,
// bracketEnd) would reproduce it byte-for-byte.
//
// Allocation-free and segment-wise: it walks the existing body against the
// escaped slug prefix, then the escaped title, then the display suffix, rather
// than concatenating them into a candidate string — which would reintroduce
// the per-bracket allocation the projection path must not have (codex R5).
func bracketUnchanged(content string, position, bracketEnd int, qualified bool, esc TitleEscaper, displaySuffix string) bool {
	body := content[position+2 : bracketEnd-2]
	if len(body) != esc.segmentLen(qualified)+len(displaySuffix) {
		return false
	}
	if qualified {
		if len(body) < esc.slugLen+1 {
			return false
		}
		if body[:esc.slugLen] != esc.escSlug || body[esc.slugLen] != '/' {
			return false
		}
		body = body[esc.slugLen+1:]
	}
	if len(body) < esc.titleLen || body[:esc.titleLen] != esc.escTitle {
		return false
	}
	return body[esc.titleLen:] == displaySuffix
}

// bracketRewriteAt is the per-bracket decision, and the ONLY copy of it. It
// returns the replacement text for the bracket at `position` (including its
// enclosing `[[`/`]]`), the offset just past the bracket it replaces, and
// whether the bracket matched at all.
//
// It reads only content[position:bracketEnd] — that locality is what makes a
// single pass possible, since the rest of the body is pure carry.
func bracketRewriteAt(content string, position int, targetTitle, collSlug string) (qualified bool, displaySuffix string, bracketEnd int, ok bool) {
	if position < 0 || position+2 > len(content) {
		return false, "", 0, false
	}
	if content[position:position+2] != "[[" {
		return false, "", 0, false
	}
	body, end, found := scanBracketBody(content, position)
	if !found {
		return false, "", 0, false
	}
	bracketEnd = end

	// MATCH AGAINST THE UNESCAPED BODY (BUG-2805 direction 1). The stored
	// bracket for a title containing `]`, `|` or `\` is escape-encoded, so
	// comparing the RAW body against the plain title never matched and the
	// link was left stale while the index row silently flipped to broken.
	//
	// Order mirrors the parser and the renderer, which both prefer the FULL
	// body as a title before splitting on a pipe (wiki_links.go's title
	// fallback, markdown.ts resolveWikiBody). Getting this order wrong would
	// break literal-pipe titles like "A|B", which case 1 has always handled.
	//
	// Nothing proportional to the new title is built here — this returns a
	// DESCRIPTION and lets each caller measure it or write it (codex R5).
	switch {
	case strings.EqualFold(unescapeWikiBody(body), targetTitle):
		// Case 1: the whole body is the title. No display segment.
	default:
		rawKey, _, hasPipe := splitOnUnescapedPipe(body)
		if !hasPipe || !strings.EqualFold(unescapeWikiBody(rawKey), targetTitle) {
			// Bracket doesn't match the expected shape — leave it alone.
			// (Index drift, or a body whose title segment is something else;
			// the trailing replaceWikiLinks call will reconcile the index.)
			return false, "", 0, false
		}
		// Case 2: title segment matches; everything from the unescaped pipe
		// onward carries over VERBATIM, still escape-encoded. It is a slice of
		// body, never a copy, and it is deliberately not re-escaped — it is
		// already in stored form.
		displaySuffix = body[len(rawKey):]
	}

	// Only now, on a confirmed match: does the `<collSlug>/` prefix carry over?
	// A qualified body like `[[tasks/Old Title]]` becomes `[[tasks/New Title]]`
	// — the cascade SELECT already proved this row points at the renamed item
	// via stage-2 qualified-fallback resolution, so the slug is guaranteed to
	// match collSlug. Reported as a flag; the caller writes the escaped slug
	// and "/" directly rather than receiving them spliced onto the title.
	if collSlug != "" && len(targetTitle) > len(collSlug) &&
		targetTitle[len(collSlug)] == '/' &&
		strings.EqualFold(targetTitle[:len(collSlug)], collSlug) {
		qualified = true
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
