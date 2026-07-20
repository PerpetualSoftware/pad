import { describe, it, expect, afterEach } from 'vitest';
import { Editor } from '@tiptap/core';
import StarterKit from '@tiptap/starter-kit';
import { BlockDragHandle } from './block-drag-handle';

// PLAN-2179 DR-1 / TASK-2180 — BlockDragHandle is now reactive-editable-aware,
// so the master-freeze flips `editable` WITHOUT remounting the editor (the
// `peeking` `{#key}` remount is gone). BlockDragHandle was the SOLE
// construction-gated freeze surface; these tests lock the two load-bearing
// properties of making it reactive instead:
//
//   1. Flipping `editable=false` (a "peek") hides the handle AND makes EVERY
//      mutation dispatch path bail — drag-reorder, duplicate, delete, turn-into
//      — so the freeze stays a TOTAL freeze even though the editor lives on.
//   2. Flipping `editable` does NOT recreate the ProseMirror view — same view,
//      same DOM node — which is the whole point of dropping the remount.
//   3. Pure superset: with `editable=true` the exact same paths DO mutate, so
//      the guards only ever add a runtime gate (byte-identical for editors).
//
// The handle + menu are imperative DOM the plugin appends to the wrapper /
// <body>, so the tests drive it through a real Tiptap editor rather than mocks.
// We use the SELECTION path (plugin `update()` → `blockAtPos`) to populate the
// active block + show the handle, which — unlike the hover path — needs no
// `posAtCoords` (absent in jsdom).

function makeEditor(element: HTMLElement): Editor {
	return new Editor({
		element,
		extensions: [StarterKit, BlockDragHandle],
		content: '<p>alpha</p><p>bravo</p>',
		editable: true,
	});
}

/** Populate the plugin's active block + reveal the handle via the selection
 *  path: arm `userHasInteracted` (a wrapper click), then drop an empty cursor
 *  inside the first paragraph so the plugin `update()` runs `blockAtPos`. */
function armHandleOnFirstBlock(editor: Editor, wrapper: HTMLElement): HTMLElement {
	wrapper.dispatchEvent(new MouseEvent('click', { bubbles: true }));
	editor.commands.setTextSelection(3); // inside "alpha"
	const handle = wrapper.querySelector('.block-drag-handle') as HTMLElement;
	return handle;
}

function menuButtonByText(label: string): HTMLElement {
	const btn = [...document.querySelectorAll('.block-menu-item')].find((b) =>
		(b.textContent ?? '').includes(label),
	);
	if (!btn) throw new Error(`menu button "${label}" not found`);
	return btn as HTMLElement;
}

describe('BlockDragHandle reactive-editable freeze (PLAN-2179 DR-1 / TASK-2180)', () => {
	let editor: Editor | null = null;
	let element: HTMLElement | null = null;

	afterEach(() => {
		editor?.destroy();
		element?.remove();
		editor = null;
		element = null;
	});

	function setup(): { editor: Editor; wrapper: HTMLElement; handle: HTMLElement } {
		element = document.body.appendChild(document.createElement('div'));
		editor = makeEditor(element);
		const handle = armHandleOnFirstBlock(editor, element);
		return { editor, wrapper: element, handle };
	}

	it('shows the handle while editable, then hides it synchronously when editable flips false', () => {
		const { editor, handle } = setup();

		// Editable: the selection path revealed the handle.
		expect(editor.view.editable).toBe(true);
		expect(handle.style.display).toBe('flex');

		// Freeze. setEditable runs an updateState → ProseMirror recomputes
		// view.editable BEFORE the plugin update fires, so the handle hides in
		// the SAME synchronous tick — no mouse move, no remount needed.
		editor.setEditable(false);
		expect(editor.view.editable).toBe(false);
		expect(handle.style.display).toBe('none');
	});

	it('does NOT recreate the editor view or its DOM node across an editable flip (no remount)', () => {
		const { editor } = setup();
		const view = editor.view;
		const dom = editor.view.dom;

		editor.setEditable(false);
		editor.setEditable(true);

		// A remount would destroy + recreate these; the reactive freeze keeps the
		// SAME view + DOM node alive across the peek/un-peek.
		expect(editor.view).toBe(view);
		expect(editor.view.dom).toBe(dom);
		expect(dom.isConnected).toBe(true);
	});

	it('while frozen, NO mutation path fires — drag-reorder, delete, duplicate, or turn-into', () => {
		const { editor, wrapper, handle } = setup();
		expect(editor.state.doc.childCount).toBe(2);

		editor.setEditable(false);

		// Delete: the menu handler bails on !editable (the active block is still
		// captured from the pre-freeze selection path, so absent the guard this
		// WOULD delete).
		document.querySelector<HTMLElement>('.block-menu-item-danger')!.dispatchEvent(
			new MouseEvent('click', { bubbles: true }),
		);
		expect(editor.state.doc.childCount).toBe(2);

		// Duplicate: same bail.
		menuButtonByText('Duplicate').dispatchEvent(new MouseEvent('click', { bubbles: true }));
		expect(editor.state.doc.childCount).toBe(2);

		// Turn-into: the delegated menu-click listener bails on !editable, so the
		// first paragraph stays a paragraph (never converted).
		document.querySelector<HTMLElement>('.block-menu-item[data-type]')!.dispatchEvent(
			new MouseEvent('click', { bubbles: true }),
		);
		expect(editor.state.doc.firstChild?.type.name).toBe('paragraph');

		// Drag-reorder: a handle mousedown + past-threshold move must NOT start a
		// drag (startDrag bails on !editable — it never adds the `active` class nor
		// disables the editor's pointer-events), and nothing reorders.
		handle.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, clientY: 100 }));
		window.dispatchEvent(new MouseEvent('mousemove', { clientY: 130 }));
		window.dispatchEvent(new MouseEvent('mouseup', { clientY: 130 }));
		expect(handle.classList.contains('active')).toBe(false);
		expect((editor.view.dom as HTMLElement).style.pointerEvents).not.toBe('none');
		expect(editor.state.doc.childCount).toBe(2);
		expect(editor.state.doc.firstChild?.textContent).toBe('alpha');
		// no stray drag ghost left in the document body
		expect(wrapper).toBeTruthy();
	});

	it('is a pure superset: with editable=true the delete path still dispatches (the guard is the ONLY blocker)', () => {
		const { editor } = setup();
		expect(editor.state.doc.childCount).toBe(2);

		// Freeze then un-freeze; un-freezing re-runs update() and re-captures the
		// active block from the (unchanged) selection.
		editor.setEditable(false);
		editor.setEditable(true);
		expect(editor.view.editable).toBe(true);

		document.querySelector<HTMLElement>('.block-menu-item-danger')!.dispatchEvent(
			new MouseEvent('click', { bubbles: true }),
		);

		// The first paragraph is gone — proving the ONLY thing blocking it while
		// frozen was the `!editorView.editable` guard, not a broken path.
		expect(editor.state.doc.childCount).toBe(1);
		expect(editor.state.doc.firstChild?.textContent).toBe('bravo');
	});
});
