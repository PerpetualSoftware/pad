import { describe, it, expect } from 'vitest';
import { wikiLinksToMarkdown, markdownToWikiLinks } from './markdown';
import type { Item } from '$lib/types';

// BUG-3315, at the string level. This began as a CHARACTERIZATION pinning the
// defect (#1684); U2 flipped the first two cases to the fixed answer, as that
// version's header said to. The editor-level legs live in
// markdown.renameMarker.svelte.test.ts.
//
// THE DEFECT, for the record: a tab loading an item turns each wiki link into a link mark whose TEXT is the
// target's title at that moment (wikiLinksToMarkdown), and from then on that text
// lives in the collaborative document. After the target is renamed, the next tab
// replays the same document, and its flush (markdownToWikiLinks against the
// post-rename index) sees text that no longer equals the title. So it stores a
// display OVERRIDE, pinning the old title for good.
//
// THE FIX: the load marks a link that follows the title with the link's `title`
// attribute (the text as loaded), and the save treats text still equal to it as
// following the title.
//
// Both stored spellings behave the same. `[[Old Title]]` is the form the
// server's title-rename cascade rewrites; `[[TASK-1]]` is a form it never
// touches. So the cascade neither causes nor prevents this (BUG-3313,
// checkpoint 4). With no op-log, the next tab seeds from the stored body and
// the link follows the title (the third case), which is why the bug needs an
// op-logged item.
//
// The editor's own serializer step (link mark to `[text](href)`) is not
// exercised here. Its round-trip fidelity comes from the 9,002-body census
// (TASK-2198).

function taskTitled(title: string): Item {
	return {
		id: 'id-1',
		title,
		slug: 'old-title',
		collection_slug: 'tasks',
		collection_prefix: 'TASK',
		item_number: 1
	} as unknown as Item;
}

const before = [taskTitled('Old Title')];
const after = [taskTitled('New Title')];

describe('a rename no longer pins the old title on an op-logged item (BUG-3315)', () => {
	for (const stored of ['see [[Old Title]] here', 'see [[TASK-1]] here']) {
		it(`document seeded before the rename from ${JSON.stringify(stored)}`, () => {
			const doc = wikiLinksToMarkdown(stored, before, 'ws', 'u');
			// Guard: the link text in the document is the pre-rename title, and it
			// carries the marker. Without this, a change to the seed could make the
			// next assertion pass vacuously.
			expect(doc).toBe('see [Old Title](/u/ws/tasks/TASK-1 "pad-follows-title:Old Title") here');
			expect(markdownToWikiLinks(doc, after)).toBe('see [[TASK-1]] here');
		});
	}

	it('no op-log: a tab seeded after the rename flushes a title-following link', () => {
		const cascaded = 'see [[New Title]] here';
		const doc = wikiLinksToMarkdown(cascaded, after, 'ws', 'u');
		expect(markdownToWikiLinks(doc, after)).toBe('see [[TASK-1]] here');
	});
});
