// The editor's own serialization of a markdown string (BUG-3197): what the
// editor emits for that markdown when nobody edits it. The editor's document is
// never touched.
//
// It replays what `setContent` does, on a DETACHED state: parse with the live
// editor's markdown parser and schema, replace the document in one transaction,
// and apply it to a state carrying the live editor's plugins, so every
// appendTransaction plugin runs as it does on the real path. That matters:
// parsing alone (createDocument) diverged from the real path on 28 of 9,002
// live bodies, all through appendTransaction plugins (the link autolink turns a
// bare URL into `<url>`; table normalisation drops the empty table a stray
// `<Table>` in prose parses to). The census and the parity pin on BUG-3197's
// trail measure this against the real path through the real Editor.svelte
// mount.
//
// The Yjs plugins are left out. They bind the live editor to its shared
// document, and a detached state must not reach it; they do not reshape
// content on a local transaction. So is Collaboration's `filterInvalidContent`
// (present only with `enableContentCheck`): when it rejects a transaction it
// disables collaboration and emits `contentError` on the LIVE editor.
//
// A transaction some other filterTransaction plugin refuses leaves the empty
// document, and serializing that would make "" the canonical form of a body
// that is not empty — so a user who deleted the whole body would be deduped
// and the deletion lost (codex round 4). A refused replace answers null, and
// the flush then compares against the seed alone, as it did before.
import { createDocument, type Editor } from '@tiptap/core';
import { EditorState, type Plugin } from '@tiptap/pm/state';

interface MarkdownStorage {
	parser: { parse(content: string): string };
	serializer: { serialize(doc: unknown): string };
}

// PluginKey names are suffixed with `$` (and a counter when reused).
const EXCLUDED_PLUGIN_KEY = /^(y-sync|y-undo|yjs-cursor|collaborationCaretAwarenessListener|filterInvalidContent)\$/;

function isExcluded(plugin: Plugin): boolean {
	return EXCLUDED_PLUGIN_KEY.test((plugin as unknown as { key: string }).key);
}

export function canonicalEditorMarkdown(editor: Editor, markdown: string): string | null {
	const storage = (editor.storage as unknown as { markdown: MarkdownStorage }).markdown;
	const parsed = createDocument(storage.parser.parse(markdown), editor.schema, {}, { errorOnInvalidContent: false });
	const plugins = editor.state.plugins.filter((p) => !isExcluded(p));
	const empty = EditorState.create({ schema: editor.schema, plugins });
	// setContent's own step: replace the whole document in one transaction.
	// applyTransaction runs every plugin's appendTransaction to a fixed point,
	// and reports a refused transaction by leaving it out of `transactions`.
	const tr = empty.tr.replaceWith(0, empty.doc.content.size, parsed.content);
	const result = empty.applyTransaction(tr);
	if (result.transactions[0] !== tr) return null;
	return storage.serializer.serialize(result.state.doc);
}
