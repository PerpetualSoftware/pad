// Editor-markdown-space dedupe predicate for the collab snapshot-flush
// (BUG-1941, regression of BUG-1899). Extracted as a pure function so the
// invariant that broke silently last time — a no-edit view must never
// re-PATCH — is pinned by a unit-testable truth table instead of living
// only inline inside the delicate +page.svelte flush path.
//
// Why this check exists at all: `runCollabFlush`'s storage-space dedupe
// (`(lastFlushedContent ?? baseline) === toSave`) re-serializes the
// editor's markdown through `markdownToWikiLinks` using the FLUSH-TIME
// wiki-link index before comparing. If that index differs from the index
// used to seed the Y.Doc (or the stored link was already non-canonical),
// the re-serialization diverges from the stored baseline even though the
// user made no edit, and a spurious PATCH fires. Comparing in
// editor-markdown-space — BEFORE `markdownToWikiLinks` ever runs — sidesteps
// the index dependency entirely for a pure view.
//
// Scope: only ever applies to the `baseline` arm of the existing dedupe,
// i.e. before this session's first successful flush. `lastFlushedContent`
// non-null means a real flush already happened this session; from then on
// the server holds edited content, so reverting to the seed must still
// PATCH (revert-safety) — hence the `lastFlushedContent === null` gate.
export function shouldDedupeEditorSpace(
	lastFlushedContent: string | null,
	seedMarkdown: string | null,
	normalizedMarkdown: string,
): boolean {
	return (
		lastFlushedContent === null &&
		seedMarkdown !== null &&
		normalizedMarkdown === seedMarkdown
	);
}
