import { describe, it, expect } from 'vitest';
import { createHeadlessEditor, flushPipeline } from '$lib/collab/materializer/entry';
import { wikiLinksToMarkdown } from './markdown';
import type { Item } from '$lib/types';

// BUG-3315 U1: a wiki link whose text contains a character the markdown pipeline
// escapes or parses must survive a tab's load-and-save UNCHANGED.
//
// This runs through the REAL shared pipeline, the functions both a tab's flush
// and the server materializer (TASK-2198) use: wikiLinksToMarkdown at load, the
// headless editor with the live extension set, then flushPipeline
// (markdownToWikiLinks + cleanBrokenLinks) at save.
//
// It was red before the fix, in both directions:
//   - An AUTO link, stored as [[TASK-1]] and following the title, came back as a
//     mangled OVERRIDE: `a > b` as [[TASK-1|a &gt; b]], `*` as `\\*`. `<topic>`
//     was DROPPED at load, because the html-enabled parser read it as a tag. The
//     first save of any item linking such a title pinned the mangled text, with
//     no rename involved. A census found 76 stored overrides of this shape.
//   - A DELIBERATE override containing the same characters was rewritten on save.
// The `&` and quote rows passed before the fix too, and stay as controls: the
// load escapes `&` now, and they pin that the escape does not break them.
// Reverting only the load half fails 7 of these legs; reverting only the flush
// half fails 15.

const OTHER_TITLE = 'Some unrelated current title';

function item(title: string): Item {
	return {
		id: 'id-1',
		title,
		slug: 'fixture',
		collection_slug: 'tasks',
		collection_prefix: 'TASK',
		item_number: 1
	} as unknown as Item;
}

// One tab session: load the stored body with `title` current, save it back.
function loadAndSave(stored: string, title: string): { text: string; flushed: string } {
	const ed = createHeadlessEditor();
	try {
		ed.commands.setContent(wikiLinksToMarkdown(stored, [item(title)], 'ws', 'u'));
		const md = (ed.storage as { markdown: { getMarkdown(): string } }).markdown.getMarkdown();
		return { text: ed.getText(), flushed: flushPipeline(md, [item(title)] as never) };
	} finally {
		ed.destroy();
	}
}

const titles: Array<[string, string]> = [
	['greater-than', 'a > b'],
	['less-than', 'a < b'],
	['asterisk', 'on /auth/* endpoints'],
	['emphasis-shaped asterisks', 'the *real* one'],
	['html-tag-shaped text', 'pad help <topic> topics'],
	['ampersand', 'x & y'],
	['double quotes', 'say "hi" twice'],
	['single quote', "it's done"],
	['everything at once', 'a<b> & "c" *d* > e'],
];

describe('BUG-3315 U1: link text with markdown/HTML-significant characters survives a save', () => {
	for (const [name, title] of titles) {
		it(`auto link follows the title (${name})`, () => {
			const { text, flushed } = loadAndSave('see [[TASK-1]] here', title);
			// The load must render the whole title; a dropped `<topic>` fails here.
			expect(text).toBe(`see ${title} here`);
			expect(flushed).toBe('see [[TASK-1]] here');
		});

		it(`deliberate override is kept byte-identical (${name})`, () => {
			const stored = `see [[TASK-1|${title}]] here`;
			const { text, flushed } = loadAndSave(stored, OTHER_TITLE);
			expect(text).toBe(`see ${title} here`);
			expect(flushed).toBe(stored);
		});
	}

	// The two legs above could both pass with an override that is merely STABLE
	// rather than correct, so a second save of the first save must also change
	// nothing.
	it('a second save is a fixed point', () => {
		for (const [, title] of titles) {
			const once = loadAndSave(`see [[TASK-1|${title}]] and [[TASK-1]]`, OTHER_TITLE).flushed;
			expect(loadAndSave(once, OTHER_TITLE).flushed).toBe(once);
		}
	});

	// KNOWN LIMITATION, pinned rather than hidden. The serializer writes a literal
	// `&gt;` and a literal `>` as the same bytes, so after a save no reading of the
	// markdown can tell them apart, and the flush takes the decoded one. A title
	// containing entity-SHAPED text therefore still becomes an override. The load is
	// correct, and the second save is a fixed point. Receipt: 0 of 9,620 live titles
	// contained `&lt;` / `&gt;` / `&amp;` / `&quot;` / `&#39;` when this was written,
	// against 1,304 containing one of < > & * _ ~ `.
	it('entity-shaped title text: load is exact, save still overrides (limitation)', () => {
		const title = 'literally &gt; here';
		const auto = loadAndSave('see [[TASK-1]] here', title);
		expect(auto.text).toBe(`see ${title} here`);
		expect(auto.flushed).toBe('see [[TASK-1|literally > here]] here');
		expect(loadAndSave(auto.flushed, title).flushed).toBe(auto.flushed);
	});

	// Control: text that genuinely differs from the title is still an override.
	it('an edited display text still becomes an override', () => {
		const { flushed } = loadAndSave('see [[TASK-1|a > c]] here', 'a > b');
		expect(flushed).toBe('see [[TASK-1|a > c]] here');
	});
});
