import { describe, it, expect } from 'vitest';
import { createHeadlessEditor, flushPipeline } from '$lib/collab/materializer/entry';
import { wikiLinksToMarkdown } from './markdown';
import type { Item } from '$lib/types';

// BUG-3315 U2: a link that FOLLOWS its target's title keeps following it across
// a rename, even on an item whose collaborative document froze the old text.
//
// A tab loads `[[TASK-1]]` as a link whose text is the title at that moment,
// and the text then lives in the document. Before U2, the next save compared
// that text with the CURRENT title only, so after a rename it wrote
// `[[TASK-1|Old Title]]` and pinned the old title for good. Now the load marks a
// title-following link with the link mark's existing `title` attribute (the
// text as loaded, so no schema change). The save then treats text that still
// equals the marker as "follows the title". A deliberate `[[REF|X]]` loads
// without the marker, so it survives a rename unchanged, as it should.
//
// Every leg goes through the REAL shared pipeline: the load, the headless
// editor with the live extension set, and flushPipeline. That is the same code
// the server materializer (TASK-2198) runs.

function task(title: string): Item {
	return {
		id: 'id-1',
		title,
		slug: 'fixture',
		collection_slug: 'tasks',
		collection_prefix: 'TASK',
		item_number: 1
	} as unknown as Item;
}

type Doc = ReturnType<typeof createHeadlessEditor>;

// Load `stored` with the item titled `loadTitle`, let `edit` touch the document
// (a tab's session), then serialize it: the op-log's frozen document.
function openAndEdit(stored: string, loadTitle: string, edit?: (ed: Doc) => void) {
	const ed = createHeadlessEditor();
	try {
		ed.commands.setContent(wikiLinksToMarkdown(stored, [task(loadTitle)], 'ws', 'u'));
		edit?.(ed);
		return {
			md: (ed.storage as { markdown: { getMarkdown(): string } }).markdown.getMarkdown(),
			html: ed.getHTML()
		};
	} finally {
		ed.destroy();
	}
}

// A later tab replays that document and saves it against `index`.
const saveWith = (md: string, index: Item[]) => flushPipeline(md, index as never);

describe('BUG-3315 U2: a rename does not pin the old title', () => {
	it('an auto link follows a rename', () => {
		const { md } = openAndEdit('see [[TASK-1]] here', 'Old Title');
		expect(saveWith(md, [task('New Title')])).toBe('see [[TASK-1]] here');
	});

	it('a legacy title link follows a rename too', () => {
		const { md } = openAndEdit('see [[Old Title]] here', 'Old Title');
		expect(saveWith(md, [task('New Title')])).toBe('see [[TASK-1]] here');
	});

	it('a deliberate override survives a rename unchanged', () => {
		const { md } = openAndEdit('see [[TASK-1|my label]] here', 'Old Title');
		expect(saveWith(md, [task('New Title')])).toBe('see [[TASK-1|my label]] here');
	});

	it('a deliberate override that equals the old title is still deliberate', () => {
		const { md } = openAndEdit('see [[TASK-1|Old Title]] here', 'Old Title');
		expect(saveWith(md, [task('New Title')])).toBe('see [[TASK-1|Old Title]] here');
	});

	it('editing an auto link\'s text makes an override', () => {
		const { md } = openAndEdit('see [[TASK-1]] here', 'Old Title', (ed) => {
			// Replace the link's text "Old Title" (positions 5..14) with "Edited".
			ed.commands.insertContentAt({ from: 5, to: 14 }, 'Edited');
		});
		expect(saveWith(md, [task('New Title')])).toBe('see [[TASK-1|Edited]] here');
	});

	for (const title of ['say "hi" twice', 'a > b & c', 'back\\slash "q"', 'on /auth/* <topic>']) {
		it(`the marker survives a title with special characters (${JSON.stringify(title)})`, () => {
			const { md } = openAndEdit('see [[TASK-1]] here', title);
			expect(saveWith(md, [task('New Title')])).toBe('see [[TASK-1]] here');
			expect(saveWith(md, [task(title)])).toBe('see [[TASK-1]] here');
		});
	}

	it('a target missing at save time does not leak the marker into stored content', () => {
		const { md } = openAndEdit('see [[TASK-1]] here', 'Old Title');
		expect(saveWith(md, [])).toBe('see [Old Title](/u/ws/tasks/TASK-1) here');
	});

	it('the editor DOM shows no tooltip from the frozen title', () => {
		const { html } = openAndEdit('see [[TASK-1]] here', 'Old Title');
		expect(html).toContain('data-href="/u/ws/tasks/TASK-1"');
		expect(html).not.toContain('title=');
	});

	// Guard (codex r1): the save's link match must not narrow what it accepted
	// before the marker existed, e.g. an href whose last segment has a space.
	it('a link to a slug with a space still converts, marker or not', () => {
		const spaced = { ...task('Old Title'), slug: 'my slug', item_number: undefined } as unknown as Item;
		expect(saveWith('see [Old Title](/u/ws/tasks/my slug) here', [spaced])).toBe('see [[Old Title]] here');
		expect(saveWith('see [Old Title](/u/ws/tasks/my slug "Old Title") here', [{ ...spaced, title: 'New Title' } as Item])).toBe('see [[New Title]] here');
	});

	it('with no rename, nothing changes', () => {
		const { md } = openAndEdit('a [[TASK-1]] and [[TASK-1|label]] b', 'Same');
		expect(saveWith(md, [task('Same')])).toBe('a [[TASK-1]] and [[TASK-1|label]] b');
	});
});
