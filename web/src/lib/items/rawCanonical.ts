// The form an OPEN rich tab stores for markdown a raw save sent (BUG-3540).
//
// With a rich tab open, a raw save goes through that tab: the applier runs
// `editor.commands.setContent(markdown)` and the tab's next collab flush stores
// `flushPipeline(getMarkdown(), localIndex)`. That is the BUG-2995 round trip,
// which rewrites bullets, emphasis and blank lines, so the stored body is our
// text in the editor's form, not byte for byte. Measured: '* one' lines beside
// a rich tab drew the stale dialog on every save after the first.
//
// This runs the same two steps on a headless editor built from the
// materializer's extension list (parity-tested against the live editor) and
// the same flush pipeline. Loaded on demand: only a refused raw save needs it.
// A failure answers null, which only means "no canonical form", so the caller
// asks the user (a false dialog is the safe direction).

import type { Editor } from '@tiptap/core';
import type { LinkEntry } from '$lib/collab/materializer/headless';

let editorPromise: Promise<{ editor: Editor; flush: (raw: string, index: readonly LinkEntry[]) => string }> | null = null;

function load() {
	if (!editorPromise) {
		editorPromise = import('$lib/collab/materializer/headless').then((m) => ({
			editor: m.createHeadlessEditor(),
			flush: m.flushPipeline,
		}));
		// A failed load is not cached: the next refusal tries again.
		editorPromise.catch(() => {
			editorPromise = null;
		});
	}
	return editorPromise;
}

/** The stored forms an open rich tab would write for each of `texts`. */
export async function openTabForms(texts: readonly string[], index: readonly LinkEntry[]): Promise<string[]> {
	let loaded;
	try {
		loaded = await load();
	} catch {
		return [];
	}
	const out: string[] = [];
	for (const text of texts) {
		try {
			loaded.editor.commands.setContent(text);
			// eslint-disable-next-line @typescript-eslint/no-explicit-any
			const md: string = (loaded.editor.storage as any).markdown.getMarkdown();
			out.push(loaded.flush(md, index));
		} catch {
			// No canonical form for this text: it can still match exactly.
		}
	}
	return out;
}
