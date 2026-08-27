package links

import (
	"regexp"
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
func RewriteBracketAt(content string, position int, targetTitle, newTitle, collSlug string) string {
	if position < 0 || position+2 > len(content) {
		return content
	}
	if content[position:position+2] != "[[" {
		return content
	}
	rest := content[position+2:]
	closeIdx := strings.Index(rest, "]]")
	if closeIdx < 0 {
		return content
	}
	body := rest[:closeIdx]
	bracketEnd := position + 2 + closeIdx + 2 // past `]]`

	tLower := strings.ToLower(body)
	ttLower := strings.ToLower(targetTitle)

	// Compose the new title segment. If target_title starts with
	// `<collSlug>/`, preserve that prefix on output so a qualified
	// body like `[[tasks/Old Title]]` becomes `[[tasks/New Title]]`
	// — the cascade SELECT already proved this row points at the
	// renamed item via stage-2 qualified-fallback resolution, so
	// the slug is guaranteed to match collSlug.
	newSegment := newTitle
	if collSlug != "" {
		pfx := collSlug + "/"
		if strings.HasPrefix(strings.ToLower(targetTitle), strings.ToLower(pfx)) {
			newSegment = pfx + newTitle
		}
	}

	// Case 1: body equals target_title (case-insensitive) — no
	// display segment, replace whole.
	if tLower == ttLower {
		return content[:position] + "[[" + newSegment + "]]" + content[bracketEnd:]
	}

	// Case 2: body starts with target_title + "|" — display segment
	// follows. Preserve everything from the pipe onward verbatim.
	if strings.HasPrefix(tLower, ttLower+"|") {
		displaySuffix := body[len(targetTitle):] // includes the pipe
		return content[:position] + "[[" + newSegment + displaySuffix + "]]" + content[bracketEnd:]
	}

	// Bracket doesn't match the expected shape — leave it alone.
	// (Index drift or a stored full-body title with embedded pipe;
	// the trailing replaceWikiLinks call will reconcile.)
	return content
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
