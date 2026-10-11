import { Extension } from '@tiptap/core';
import { Plugin, PluginKey, type Transaction } from '@tiptap/pm/state';
import { AddMarkStep, AddNodeMarkStep, ReplaceStep, ReplaceAroundStep } from '@tiptap/pm/transform';
import type { Node as PMNode, Fragment } from '@tiptap/pm/model';

/**
 * No marks on inline atom nodes (BUG-3568).
 *
 * ProseMirror lets a mark sit on an inline atom (a hard break, an inline
 * attachment): bolding, linking or striking a range that contains one marks
 * it, and markdown or HTML parsing marks it when the source wraps it
 * (`**a\
 * b**`). y-tiptap never writes marks on non-text inline nodes to the Y.Doc,
 * so the tab holding such a mark had a document its own Y.Doc did not: it
 * flushed one body while every other tab, a reload and the materializer
 * derived another. This strips those marks in the same update, so the editor
 * always equals its Y.Doc.
 *
 * A plugin rather than a node spec: NodeSpec.marks governs the marks allowed
 * on a node's CONTENT, not on the node itself, so `marks: ''` on a leaf does
 * nothing (measured). No node spec or persisted shape changes.
 */
export const inlineAtomNoMarksKey = new PluginKey('inlineAtomNoMarks');

const isMarkedAtom = (n: PMNode) => n.isInline && !n.isText && n.marks.length > 0;

function fragmentHasMarkedAtom(f: Fragment): boolean {
	let found = false;
	f.descendants((n) => {
		if (found) return false;
		if (isMarkedAtom(n)) found = true;
		return !found;
	});
	return found;
}

/** Whether a transaction could have put a mark on an inline atom. */
function mayMarkAtoms(tr: Transaction): boolean {
	return tr.steps.some(
		(s) =>
			s instanceof AddMarkStep ||
			s instanceof AddNodeMarkStep ||
			((s instanceof ReplaceStep || s instanceof ReplaceAroundStep) &&
				fragmentHasMarkedAtom((s as unknown as { slice: { content: Fragment } }).slice.content)),
	);
}

export const InlineAtomNoMarks = Extension.create({
	name: 'inlineAtomNoMarks',
	addProseMirrorPlugins() {
		return [
			new Plugin({
				key: inlineAtomNoMarksKey,
				appendTransaction(trs, _old, state) {
					if (!trs.some((tr) => tr.docChanged && mayMarkAtoms(tr))) return null;
					let tr: Transaction | null = null;
					state.doc.descendants((n, pos) => {
						if (isMarkedAtom(n)) {
							tr = tr ?? state.tr;
							tr.setNodeMarkup(pos, undefined, n.attrs, []);
						}
					});
					return tr;
				},
			}),
		];
	},
});
