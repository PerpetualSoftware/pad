import { it, expect } from 'vitest';
import * as Y from 'yjs';
import { Editor } from '@tiptap/core';
import Collaboration from '@tiptap/extension-collaboration';
import { TextSelection } from '@tiptap/pm/state';
import { headlessExtensions } from './materializer/headless';
import { applyPatch, type PatchContext } from './patchApply';

const ctx: PatchContext = { toCanonical: (md) => md, toEditor: (md) => md };

function collabEditor(doc: Y.Doc) {
	return new Editor({ element: document.createElement('div'), extensions: [...headlessExtensions(), Collaboration.configure({ document: doc, field: 'default' })] });
}
function sync(a: Y.Doc, b: Y.Doc) {
	Y.applyUpdate(b, Y.encodeStateAsUpdate(a, Y.encodeStateVector(b)));
	Y.applyUpdate(a, Y.encodeStateAsUpdate(b, Y.encodeStateVector(a)));
}
function textPos(ed: Editor, needle: string): number {
	let at = -1;
	ed.state.doc.descendants((n, pos) => {
		if (at < 0 && n.isText && n.text!.includes(needle)) at = pos + n.text!.indexOf(needle);
	});
	return at;
}

// PLAN-3578 step 2: the reason for a narrowed apply. A patch applied in one
// tab merges with typing another tab has not synced yet, and the other tab's
// cursor stays on its text. A whole-document setContent fails all three.
it('a patch merges with concurrent typing and leaves the other cursor on its text', () => {
	const da = new Y.Doc();
	const db = new Y.Doc();
	const a = collabEditor(da);
	const b = collabEditor(db);
	a.commands.setContent('intro\n\nmiddle\n\nending');
	sync(da, db);
	expect(b.state.doc.textContent).toBe('intromiddleending');

	// Tab B: cursor inside "ending", and unsynced typing in "intro".
	b.view.dispatch(b.state.tr.setSelection(TextSelection.create(b.state.doc, textPos(b, 'ending') + 3)));
	b.view.dispatch(b.state.tr.insertText('!!', textPos(b, 'intro') + 5));

	// Tab A (the applier) patches "middle", concurrently.
	const res = applyPatch(a, [{ old: 'middle', new: 'MIDDLE' }], ctx);
	expect(res.ok).toBe(true);

	sync(da, db);
	const want = 'intro!!MIDDLEending';
	expect(a.state.doc.textContent).toBe(want);
	expect(b.state.doc.textContent).toBe(want);
	const sel = b.state.selection.from;
	expect(b.state.doc.textBetween(sel - 3, sel + 3)).toBe('ending');
	a.destroy();
	b.destroy();
});

// The cases a narrowed patch exists for, each with its control: the old
// applier's whole-body setContent, given the agent's body built from an
// EARLIER read, under the same state. Each control goes red where the patch
// stays green, so the patch test measures the narrowed path, not Yjs.
for (const mode of ['patch', 'setContent'] as const) {
	it(`${mode}: keeps typing the applier already holds, and its own cursor`, () => {
		const da = new Y.Doc();
		const db = new Y.Doc();
		const a = collabEditor(da);
		const b = collabEditor(db);
		a.commands.setContent('intro\n\nmiddle\n\nending');
		sync(da, db);
		// The agent read the body now: "intro / middle / ending".
		const agentRead = 'intro\n\nmiddle\n\nending';
		// Then a human types in tab B, and it reaches the applier tab A.
		b.view.dispatch(b.state.tr.insertText('!!', textPos(b, 'intro') + 5));
		sync(da, db);
		// Tab A's own cursor sits inside "ending".
		a.view.dispatch(a.state.tr.setSelection(TextSelection.create(a.state.doc, textPos(a, 'ending') + 3)));

		if (mode === 'patch') {
			expect(applyPatch(a, [{ old: 'middle', new: 'MIDDLE' }], ctx).ok).toBe(true);
		} else {
			a.commands.setContent(agentRead.replace('middle', 'MIDDLE'));
		}
		sync(da, db);
		const kept = a.state.doc.textContent === 'intro!!MIDDLEending';
		const sel = a.state.selection.from;
		const cursorStayed = a.state.doc.textBetween(Math.max(0, sel - 3), Math.min(a.state.doc.content.size, sel + 3)) === 'ending';
		if (mode === 'patch') {
			expect(kept).toBe(true);
			expect(cursorStayed).toBe(true);
		} else {
			// The whole-body replace reverts the synced typing and moves the
			// cursor: the reason the applier used to refuse (BUG-3542).
			expect(kept && cursorStayed).toBe(false);
		}
		a.destroy();
		b.destroy();
	});
}
