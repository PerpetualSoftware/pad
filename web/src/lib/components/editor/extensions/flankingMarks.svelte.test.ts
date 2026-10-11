import { describe, it, expect } from 'vitest';
import { Editor } from '@tiptap/core';
import StarterKit from '@tiptap/starter-kit';
import { Markdown } from 'tiptap-markdown';
import { createHeadlessEditor } from '$lib/collab/materializer/headless';

// BUG-3583: bold, italic and strike serialize to markdown that parses back to
// the same marks. Runs through the materializer's headless editor, which uses
// the editor's extension list (htmlProseExtensions) and its parser.

type J = Record<string, unknown>;
const t = (text: string, marks: string[] = []): J => ({ type: 'text', text, ...(marks.length ? { marks: marks.map((type) => ({ type })) } : {}) });
const para = (...content: J[]): J => ({ type: 'doc', content: [{ type: 'paragraph', content }] });

function marksOf(e: Editor): string {
	const out: string[] = [];
	e.state.doc.descendants((n) => {
		if (n.isText) for (const ch of n.text ?? '') if (!/\s/.test(ch)) out.push(`${ch}:${n.marks.map((m) => m.type.name).sort().join('+')}`);
	});
	return out.join('|');
}

function roundTrip(e: Editor, doc: J): { md: string; want: string; got: string } {
	e.commands.setContent(doc);
	const want = marksOf(e);
	const md = (e.storage as { markdown: { getMarkdown(): string } }).markdown.getMarkdown();
	e.commands.setContent(md);
	return { md, want, got: marksOf(e) };
}

// The serializer this replaces, for the controls: StarterKit's marks with
// tiptap-markdown's built-in specs.
const oldEditor = () => new Editor({ element: null, extensions: [StarterKit, Markdown.configure({ html: true })], content: '' });

describe('BUG-3583: marks that cannot flank', () => {
	const ed = createHeadlessEditor();

	it("the report's case: bold ending in punctuation, then a letter", () => {
		const r = roundTrip(ed, para(t('Hello.', ['bold']), t('World')));
		expect(r.md).toBe('<strong>Hello.</strong>World');
		expect(r.got).toBe(r.want);
		// Control: the old serializer wrote literal asterisks.
		const o = roundTrip(oldEditor(), para(t('Hello.', ['bold']), t('World')));
		expect(o.md).toBe('**Hello.**World');
		expect(o.got).not.toBe(o.want);
	});

	for (const [mark, tag] of [['bold', 'strong'], ['italic', 'em'], ['strike', 's']] as const) {
		it(`${mark}: both edges, and the opening edge the old serializer moved punctuation across`, () => {
			for (const doc of [para(t('Hello.', [mark]), t('World')), para(t('Hello'), t('.World', [mark])), para(t('a'), t('(x)', [mark]), t('b'))]) {
				const r = roundTrip(ed, doc);
				expect(r.got, r.md).toBe(r.want);
				expect(r.md).toContain(`<${tag}>`);
			}
		});
	}

	it('a run that flanks keeps its markdown form, byte for byte', () => {
		expect(roundTrip(ed, para(t('Hello', ['bold']), t('World'))).md).toBe('**Hello**World');
		expect(roundTrip(ed, para(t('Plain '), t('bold', ['bold']), t(', '), t('it', ['italic']), t(' and '), t('gone', ['strike']))).md).toBe(
			'Plain **bold**, *it* and ~~gone~~',
		);
		// Edge spaces still go outside the delimiters, as before.
		expect(roundTrip(ed, para(t('Add ', ['bold']), t('more'))).md).toBe('**Add** more');
	});

	it('code needs nothing (a code span closes on its backticks)', () => {
		const r = roundTrip(ed, para(t('Hello.', ['code']), t('World')));
		expect(r.md).toBe('`Hello.`World');
		expect(r.got).toBe(r.want);
	});

	it('inside a quote with a hard break and nested marks, the line prefix survives', () => {
		const doc: J = {
			type: 'doc',
			content: [{ type: 'blockquote', content: [{ type: 'paragraph', content: [t('a', ['bold', 'strike']), t(';)', ['bold', 'italic']), { type: 'hardBreak' }, t(' ,Y;)', ['italic']), t('a !X!', ['bold', 'italic']), t(".—':", ['bold'])] }] }],
		};
		const r = roundTrip(ed, doc);
		expect(r.got, r.md).toBe(r.want);
		expect(r.md.split('\n').every((l) => l.startsWith('>'))).toBe(true);
	});

	// Seeded fuzz over the shapes that broke: letters, punctuation and spaces
	// under overlapping marks, links and hard breaks, in paragraphs, quotes and
	// list items. Every case whose TEXT round-trips must keep its marks; the
	// text itself has a separate, pre-existing line-start escaping gap.
	it('fuzz: no mark is lost, and no serializer internals leak into the text', { timeout: 60_000 }, () => {
		let s = 3583;
		const r = () => ((s = (s * 1103515245 + 12345) & 0x7fffffff) / 0x7fffffff);
		const chars = 'abXY .,:;!?()-"\'';
		const sets = [[], ['bold'], ['italic'], ['strike'], ['code'], ['bold', 'italic'], ['bold', 'strike'], ['italic', 'strike']];
		const old = oldEditor();
		let checked = 0;
		let oldLost = 0;
		for (let n = 0; n < 400; n++) {
			const content: J[] = [];
			for (let k = 2 + Math.floor(r() * 4); k > 0; k--) {
				let text = '';
				for (let c = 1 + Math.floor(r() * 5); c > 0; c--) text += chars[Math.floor(r() * chars.length)];
				const ms = sets[Math.floor(r() * sets.length)];
				const marks: J[] = ms.map((type) => ({ type }));
				if (!ms.includes('code') && r() < 0.15) marks.push({ type: 'link', attrs: { href: 'https://x.test/a' } });
				content.push({ type: 'text', text, ...(marks.length ? { marks } : {}) });
				if (r() < 0.08) content.push({ type: 'hardBreak' });
			}
			const p = { type: 'paragraph', content };
			const w = r();
			const doc = { type: 'doc', content: [w < 0.2 ? { type: 'blockquote', content: [p] } : w < 0.4 ? { type: 'bulletList', content: [{ type: 'listItem', content: [p] }] } : p] };
			const res = roundTrip(ed, doc);
			expect(res.md).not.toContain('function');
			const chars_ = (x: string) => x.split('|').map((c) => c.split(':')[0]).join('');
			if (chars_(res.got) !== chars_(res.want)) continue;
			checked++;
			expect(res.got, res.md).toBe(res.want);
			const o = roundTrip(old, doc);
			if (o.got !== o.want) oldLost++;
		}
		expect(checked).toBeGreaterThan(350);
		// Vantage point: the generator reaches the defect (the old serializer
		// loses marks on a good share of these).
		expect(oldLost).toBeGreaterThan(50);
	});

	// markdown-it counts Unicode symbols as punctuation (CommonMark 0.31), and
	// reads a surrogate pair as one character: an emoji at a run's edge is the
	// same case as a full stop.
	it('an emoji at the edge is punctuation too', () => {
		const r = roundTrip(ed, para(t('done 🎉', ['bold']), t('Next')));
		expect(r.got, r.md).toBe(r.want);
		expect(r.md).toBe('<strong>done 🎉</strong>Next');
	});
});
