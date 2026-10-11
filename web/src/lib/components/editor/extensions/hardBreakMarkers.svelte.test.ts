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

// Blocks and their text, with runs of whitespace collapsed: markdown-it drops
// the spaces that start a line after a hard break, a long-standing invisible
// normalization that is not what this bug is about.
function shape(e: Editor): string {
	const out: string[] = [];
	e.state.doc.descendants((n) => {
		if (n.isBlock) out.push(`[${n.type.name}]`);
		if (n.isText) out.push((n.text ?? '').replace(/\s+/g, ' ').trim());
	});
	return out.join('');
}

function roundTrip(e: Editor, doc: J) {
	e.commands.setContent(doc);
	const want = shape(e);
	const md = (e.storage as { markdown: { getMarkdown(): string } }).markdown.getMarkdown();
	e.commands.setContent(md);
	return { md, same: shape(e) === want, got: shape(e) };
}

describe('BUG-3584: block markers after a hard break', () => {
	const ed = createHeadlessEditor();
	for (const after of ['- not a list item', '* not a list item', '+ not a list item', '> not a quote', '# not a heading', '### nor this', '1. not an ordered item', '7) nor this', '   - indented, still a list marker', '  1. indented ordered', '---', '___', '* * *', '===', '-', '= =', '==']) {
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

	// BUG-3600: a text node's end is the end of the LINE only when nothing
	// follows it there. A marker followed directly by marked text opens no
	// block, so it keeps its form; one followed by a space still opens one.
	const split = (marker: string, marked: string): J => ({
		type: 'doc',
		content: [{ type: 'paragraph', content: [{ type: 'text', text: 'First line' }, { type: 'hardBreak' }, { type: 'text', text: marker }, { type: 'text', text: marked, marks: [{ type: 'bold' }] }] }],
	});
	it('a marker followed directly by marked text keeps its form', () => {
		for (const marker of ['-', '#', '*', '>', '1.']) {
			const r = roundTrip(ed, split(marker, 'item'));
			expect(r.same, `${marker}: ${r.md}`).toBe(true);
			// `*` beside `**` keeps prosemirror-markdown's ordinary emphasis
			// escape, which applies anywhere in a line; `>` there is not the
			// line-start marker. Neither is this escape's to decide.
			if (marker !== '>' && marker !== '*') expect(r.md, marker).not.toContain('\\' + marker.slice(-1) + '**');
		}
	});
	// A number split across nodes: the mark's delimiters sit between them
	// (`1**2. item**`), so no ordered marker forms, before or after BUG-3600.
	it('a number split across a mark boundary opens no list', () => {
		for (const [a, b] of [['1', '2. item'], ['1', '2) item'], ['12', '. item']]) {
			const r = roundTrip(ed, split(a, b));
			expect(r.same, `${a}|${b}: ${r.md}`).toBe(true);
		}
	});

	it('control: a marker followed by marked text after a space still stays text', () => {
		for (const marker of ['-', '#', '1.']) {
			const r = roundTrip(ed, split(marker, ' item'));
			expect(r.same, `${marker}: ${r.md}`).toBe(true);
		}
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

	// Only markers that would open a block are escaped: these keep their
	// exact form, so bodies that hold them do not rewrite.
	it('text that opens no block keeps its form', () => {
		for (const after of ['#tag', '-5 degrees', '1.5 litres', '+1', '=x', '>'.slice(1) + 'a > b']) {
			const r = roundTrip(ed, para(after));
			expect(r.same, `${after}: ${r.md}`).toBe(true);
			expect(r.md, after).not.toMatch(/\n\\[-#1+*=_]/);
		}
	});

	// At a true block start prosemirror-markdown's escape stays in charge, with
	// two exceptions it missed: `1)` and an indented bullet both became lists.
	it('at a block start: `1)` and an indented bullet stay text, the rest keep their old form', () => {
		const block = (text: string): J => ({ type: 'doc', content: [{ type: 'paragraph', content: [{ type: 'text', text }] }] });
		for (const text of ['1) x', '   - x']) expect(roundTrip(ed, block(text)).same, text).toBe(true);
		expect(roundTrip(ed, block('---')).md).toBe('\\---');
		expect(roundTrip(ed, block('___')).md).toBe('\\__\\_');
		expect(roundTrip(ed, block('===')).md).toBe('===');
	});
});
