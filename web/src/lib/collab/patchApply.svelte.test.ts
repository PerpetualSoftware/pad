import { describe, it, expect } from 'vitest';
import { TextSelection } from '@tiptap/pm/state';
import type { Node as PMNode } from '@tiptap/pm/model';
import { createHeadlessEditor } from './materializer/headless';
import { applyPatch, applyEditsToText, planPatch, type PatchContext } from './patchApply';

const ctx: PatchContext = { toCanonical: (md) => md, toEditor: (md) => md };

function editorWith(md: string) {
	const ed = createHeadlessEditor();
	ed.commands.setContent(md);
	return ed;
}
const blocks = (d: PMNode) => Array.from({ length: d.childCount }, (_, i) => d.child(i));

describe('PLAN-3578 step 2: narrowed apply', () => {
	it('replaces only the changed block; untouched blocks keep their identity', () => {
		const ed = editorWith('one\n\ntwo\n\nthree\n\nfour');
		const before = blocks(ed.state.doc);
		const res = applyPatch(ed, [{ old: 'three', new: 'THREE' }], ctx);
		expect(res.ok).toBe(true);
		const after = blocks(ed.state.doc);
		expect(after.map((b) => b.textContent)).toEqual(['one', 'two', 'THREE', 'four']);
		expect(after[0]).toBe(before[0]);
		expect(after[1]).toBe(before[1]);
		expect(after[3]).toBe(before[3]);
	});

	it('leaves a cursor before the change where it was, and maps one after it', () => {
		const ed = editorWith('alpha\n\nbeta\n\ngamma');
		// Cursor inside "alpha" (pos 3) and, separately, inside "gamma".
		ed.view.dispatch(ed.state.tr.setSelection(TextSelection.create(ed.state.doc, 3)));
		applyPatch(ed, [{ old: 'beta', new: 'BETA beta' }], ctx);
		expect(ed.state.selection.from).toBe(3);

		const g = ed.state.doc.textContent.indexOf('gamma');
		expect(g).toBeGreaterThan(0);
		let gammaPos = 0;
		ed.state.doc.descendants((n, pos) => {
			if (n.isText && n.text === 'gamma') gammaPos = pos + 2;
		});
		ed.view.dispatch(ed.state.tr.setSelection(TextSelection.create(ed.state.doc, gammaPos)));
		applyPatch(ed, [{ old: 'alpha', new: 'a' }], ctx);
		const sel = ed.state.selection.from;
		expect(ed.state.doc.textBetween(sel - 2, sel + 3)).toBe('gamma');
	});

	it('inserts a new block between unchanged neighbours', () => {
		const ed = editorWith('first\n\nlast');
		const first = ed.state.doc.child(0);
		const res = applyPatch(ed, [{ old: 'first', new: 'first\n\nmiddle' }], ctx);
		expect(res.ok).toBe(true);
		expect(blocks(ed.state.doc).map((b) => b.textContent)).toEqual(['first', 'middle', 'last']);
		expect(ed.state.doc.child(0)).toBe(first);
	});

	it('refuses an edit to a block that does not round-trip, and allows one elsewhere', () => {
		const ed = createHeadlessEditor();
		const s = ed.schema;
		// A block whose canonical form does not parse back to it. This used to
		// be BUG-3583's case (bold "Hello." + "World"), until that fix made it
		// round-trip; a real serializer defect is a moving fixture. Here the
		// canonical step itself drops the bold run's delimiters, so the parse of
		// the first block can never match the live one.
		const lossy: PatchContext = { toCanonical: (md) => md.replace('**Hello**', 'Hello'), toEditor: (md) => md };
		const doc = s.node('doc', null, [
			s.node('paragraph', null, [s.text('Hello', [s.marks.bold.create()]), s.text(' World')]),
			s.node('paragraph', null, [s.text('plain')]),
		]);
		ed.view.dispatch(ed.state.tr.replaceWith(0, ed.state.doc.content.size, doc.content));
		const refused = planPatch(ed, [{ old: 'World', new: 'Earth' }], lossy);
		expect(refused).toMatchObject({ ok: false, reason: 'unstable_roundtrip' });
		const kept = ed.state.doc;
		const res = applyPatch(ed, [{ old: 'plain', new: 'PLAIN' }], lossy);
		expect(res.ok).toBe(true);
		expect(ed.state.doc.child(0)).toBe(kept.child(0));
		expect(ed.state.doc.child(1).textContent).toBe('PLAIN');
	});

	it('treats whitespace bold as invisible, but not whitespace code', () => {
		const ed = createHeadlessEditor();
		const s = ed.schema;
		const bold = s.marks.bold.create();
		const code = s.marks.code.create();
		// "**Add **" — the round trip moves the space out of the bold.
		const boldSpace = s.node('paragraph', null, [s.text('Add ', [bold]), s.text('it')]);
		ed.view.dispatch(ed.state.tr.replaceWith(0, ed.state.doc.content.size, s.node('doc', null, [boldSpace]).content));
		expect(planPatch(ed, [{ old: 'it', new: 'that' }], ctx).ok).toBe(true);
		// A space inside code is visible; if its round trip differs it must refuse.
		const codeSpace = s.node('paragraph', null, [s.text('x', [code]), s.text(' ', [code]), s.text('y')]);
		ed.view.dispatch(ed.state.tr.replaceWith(0, ed.state.doc.content.size, s.node('doc', null, [codeSpace]).content));
		const plan = planPatch(ed, [{ old: 'y', new: 'z' }], ctx);
		// Either it round-trips exactly (then ok) or it is refused; never a
		// silent normalization of a visible mark.
		if (!plan.ok) expect(plan.reason).toBe('unstable_roundtrip');
		else expect(ed.state.doc.child(0).eq(codeSpace)).toBe(true);
	});

	it('answers anchor refusals without touching the editor', () => {
		const ed = editorWith('one two two');
		const doc = ed.state.doc;
		expect(applyPatch(ed, [{ old: 'three', new: '3' }], ctx)).toMatchObject({ ok: false, reason: 'anchor_missing', edit: 0 });
		expect(applyPatch(ed, [{ old: 'two', new: '2' }], ctx)).toMatchObject({ ok: false, reason: 'anchor_ambiguous', lines: [1, 1] });
		expect(applyPatch(ed, [{ old: '', new: 'x' }], ctx)).toMatchObject({ ok: false, reason: 'empty_anchor' });
		expect(ed.state.doc).toBe(doc);
	});
});

describe('applyEditsToText matches the server', () => {
	it('counts overlapping matches and honours line_hint', () => {
		expect(applyEditsToText('aaa', [{ old: 'aa', new: 'b' }])).toMatchObject({ ok: false, reason: 'anchor_ambiguous' });
		expect(applyEditsToText('x\ny x', [{ old: 'x', new: 'z', line_hint: 2 }])).toEqual({ ok: true, after: 'x\ny z' });
		expect(applyEditsToText('a\nb', [{ old: 'a', new: 'x' }, { old: 'x\nb', new: 'y' }])).toEqual({ ok: true, after: 'y' });
	});
});
