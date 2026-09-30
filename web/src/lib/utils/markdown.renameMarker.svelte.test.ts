import { describe, it, expect } from 'vitest';
import { createHeadlessEditor, flushPipeline } from '$lib/collab/materializer/entry';
import { wikiLinksToMarkdown, cleanBrokenLinks } from './markdown';
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
		expect(saveWith('see [Old Title](/u/ws/tasks/my slug "pad-follows-title:Old Title") here', [{ ...spaced, title: 'New Title' } as Item])).toBe('see [[New Title]] here');
	});

	// Guard (codex r2): a title the USER wrote on an internal link is not a
	// marker. The save leaves such a link exactly as before U2, and the DOM keeps
	// the tooltip.
	it('a user-authored titled internal link is left untouched', () => {
		const typed = 'see [x](/u/ws/tasks/TASK-1 "my tip") here';
		expect(saveWith(typed, [task('Old Title')])).toBe(typed);
		const ed = createHeadlessEditor();
		try {
			ed.commands.setContent(typed);
			expect(ed.getHTML()).toContain('title="my tip"');
			expect(saveWith((ed.storage as { markdown: { getMarkdown(): string } }).markdown.getMarkdown(), [task('Old Title')])).toBe(typed);
		} finally {
			ed.destroy();
		}
	});

	// THE INVARIANT (lead, before the GO): the marker never reaches stored
	// markdown, on ANY path where a marked link does not convert back to
	// [[...]]. Each case is a real save pipeline (flushPipeline, the same steps as
	// both ItemDetail save paths) over a document a tab really produced.
	describe('the marker never reaches stored markdown', () => {
		const other = { ...task('Unrelated'), id: 'id-2', item_number: 2, slug: 'other' } as unknown as Item;
		const hardBreak = (ed: Doc) => {
			// Shift-Enter inside the link text: a hard break splits "Old|Title".
			ed.commands.setTextSelection(8);
			ed.commands.setHardBreak();
		};
		const boldPart = (ed: Doc) => {
			ed.commands.setTextSelection({ from: 5, to: 8 });
			ed.commands.toggleBold();
		};
		const externalHref = (ed: Doc) => {
			// The user edits the auto link's URL to an external address; the link
			// mark keeps its attributes, the marker included.
			ed.chain().setTextSelection({ from: 5, to: 14 }).extendMarkRange('link').updateAttributes('link', { href: 'https://example.com/x' }).run();
		};
		const cases: Array<[string, string, Item[], ((ed: Doc) => void)?]> = [
			['target deleted (index lacks it)', 'see [[TASK-1]] here', [other]],
			['empty index (pipelines skip conversion)', 'see [[TASK-1]] here', []],
			['restricted viewer (target not visible)', 'see [[TASK-1]] here', [other]],
			['hard break inside the link text, target present', 'see [[TASK-1]] here', [task('Old Title')], hardBreak],
			['hard break inside the link text, target gone', 'see [[TASK-1]] here', [other], hardBreak],
			['partial bold inside the link, target gone', 'see [[TASK-1]] here', [other], boldPart],
			['link in a markdown table, target gone', '| a | b |\n| --- | --- |\n| [[TASK-1]] | x |', [other]],
			['link in an HTML-serialized table (list in a cell), target gone', '<table><tbody><tr><td><ul><li><p>[[TASK-1]]</p></li></ul></td></tr></tbody></table>', [other]],
			['URL edited to an external address, target present', 'see [[TASK-1]] here', [task('Old Title')], externalHref],
			['URL edited to an external address, empty index', 'see [[TASK-1]] here', [], externalHref],
		];
		for (const [name, stored, index, edit] of cases) {
			it(name, () => {
				const { md } = openAndEdit(stored, 'Old Title', edit);
				// Guard: the document really carries the marker, or the case proves nothing.
				expect(md).toContain('pad-follows-title');
				expect(saveWith(md, index)).not.toContain('pad-follows-title');
			});
		}
		// ...and the strip touches ONLY a real link's title (codex, after the
		// leak fix). Text that merely mentions the marker, as a doc about this
		// feature does, is content and must survive a save byte-identical.
		for (const [name, body] of [
			['inline code span', 'the marker is `[Title](/u/ws/tasks/TASK-1 "pad-follows-title:Title")` in storage'],
			['fenced code block', 'before\n\n```\n[T](/x "pad-follows-title:T")\n```\n\nafter'],
			['prose with escaped brackets', 'plain \\[T\\](/x "pad-follows-title:T") text'],
			['a bare mention', 'writes "pad-follows-title:x") as a title'],
		] as const) {
			it(`text that mentions the marker is untouched (${name})`, () => {
				expect(saveWith(body, [])).toBe(body);
				expect(saveWith(body, [task('Title')])).toBe(body);
			});
		}

		// Codex r2 of the code-aware strip: a link text ending in a backslash is
		// serialized `[a\\](...)`. Its `]` follows a backslash, but an even
		// number of them, so it is a real link and its marker must go.
		it('a real link whose text ends in a backslash still loses its marker', () => {
			const { md } = openAndEdit('see [[TASK-1]] here', 'C:\\');
			expect(md).toContain('pad-follows-title');
			expect(saveWith(md, [])).not.toContain('pad-follows-title');
		});

		// Codex r3: a backtick before a fence must not pair with one after it. A
		// span crossing the fence overlapped the fence's range, and the binary
		// search then misread offsets. Here the text after the fence is a real
		// inline span, and the marker-shaped text in it must survive.
		it('an inline span does not pair across a fenced block', () => {
			const body = '`\n```\ncode\n```\n` [T](/x "pad-follows-title:T") `';
			expect(saveWith(body, [])).toBe(body);
		});

		// Codex r4 (did not reproduce, pinned): runs of different lengths must not
		// form overlapping spans. In "`a ``b ` LINK `` " the single-backtick span
		// closes at the third run, and the "``" inside it is content, not an
		// opener, so LINK sits OUTSIDE code and its marker must go.
		it('a run inside a span does not open a second, overlapping span', () => {
			const body = '`a ``b ` [x](/u/ws/tasks/TASK-1 "pad-follows-title:x") ``';
			expect(saveWith(body, [])).toBe('`a ``b ` [x](/u/ws/tasks/TASK-1) ``');
		});

		// ...and the code scan is linear. Receipt, local: 40,000 alternating
		// backtick runs took 267ms under the first version (18x for 10x the input)
		// and take 9.6ms now (98ms at 400,000). The bound sits between, with room
		// for a slower CI machine.
		it('the code scan stays linear on many backtick runs', () => {
			const parts: string[] = [];
			for (let i = 0; i < 40000; i++) parts.push(i % 2 ? '`` x ' : '` x ');
			const body = parts.join('') + '[T](/u/ws/tasks/TASK-1 "pad-follows-title:T")';
			const t0 = performance.now();
			cleanBrokenLinks(body);
			expect(performance.now() - t0).toBeLessThan(150);
		});

		it('the guard is live: a marked document does carry the marker before the save', () => {
			expect(openAndEdit('see [[TASK-1]] here', 'Old Title').md).toContain('pad-follows-title');
		});
	});

	it('with no rename, nothing changes', () => {
		const { md } = openAndEdit('a [[TASK-1]] and [[TASK-1|label]] b', 'Same');
		expect(saveWith(md, [task('Same')])).toBe('a [[TASK-1]] and [[TASK-1|label]] b');
	});
});
