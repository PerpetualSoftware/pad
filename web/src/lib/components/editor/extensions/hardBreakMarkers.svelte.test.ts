import { describe, it, expect } from 'vitest';
import { Editor } from '@tiptap/core';
import StarterKit from '@tiptap/starter-kit';
import { Markdown } from 'tiptap-markdown';
import { createHeadlessEditor } from '$lib/collab/materializer/headless';

// BUG-3584: text after a hard break that starts with a block marker must stay
// text. prosemirror-markdown escaped those markers only at a block start, so
// `- not a list item` after a hard break read back as a bullet list.

type J = Record<string, unknown>;
const para = (after: string): J => ({
	type: 'doc',
	content: [{ type: 'paragraph', content: [{ type: 'text', text: 'First line' }, { type: 'hardBreak' }, { type: 'text', text: after }] }],
});

function roundTrip(e: Editor, doc: J) {
	e.commands.setContent(doc);
	const want = JSON.stringify(e.getJSON());
	const md = (e.storage as { markdown: { getMarkdown(): string } }).markdown.getMarkdown();
	e.commands.setContent(md);
	return { md, same: JSON.stringify(e.getJSON()) === want, got: e.getJSON() };
}

describe('BUG-3584: block markers after a hard break', () => {
	const ed = createHeadlessEditor();
	for (const after of ['- not a list item', '* not a list item', '+ not a list item', '> not a quote', '# not a heading', '1. not an ordered item']) {
		it(`${JSON.stringify(after)} stays text`, () => {
			const r = roundTrip(ed, para(after));
			expect(r.same, `${r.md} => ${JSON.stringify(r.got)}`).toBe(true);
		});
	}

	it('the report case, with a control on the old serializer', () => {
		const r = roundTrip(ed, para('- not a list item'));
		expect(r.md).toBe('First line\\\n\\- not a list item');
		const old = new Editor({ element: null, extensions: [StarterKit, Markdown.configure({ html: true })] });
		expect(roundTrip(old, para('- not a list item')).same).toBe(false);
	});

	it('mid-line markers are not escaped', () => {
		const r = roundTrip(ed, para('see a - b and #1'));
		expect(r.md).toBe('First line\\\nsee a - b and #1');
		expect(r.same).toBe(true);
	});

	it('inside a list item and a quote too', () => {
		for (const wrap of ['bulletList', 'blockquote'] as const) {
			const p = { type: 'paragraph', content: [{ type: 'text', text: 'First line' }, { type: 'hardBreak' }, { type: 'text', text: '- inner' }] };
			const doc = { type: 'doc', content: [wrap === 'bulletList' ? { type: 'bulletList', content: [{ type: 'listItem', content: [p] }] } : { type: 'blockquote', content: [p] }] };
			const r = roundTrip(ed, doc);
			expect(r.same, `${wrap}: ${r.md}`).toBe(true);
		}
	});
});
