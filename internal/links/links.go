package links

import "regexp"

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

func replaceAll(s, old, new string) string {
	// Simple string replacement, not regex-based
	result := s
	for {
		i := indexOf(result, old)
		if i < 0 {
			break
		}
		result = result[:i] + new + result[i+len(old):]
	}
	return result
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
