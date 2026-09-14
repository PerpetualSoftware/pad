import { describe, it, expect, afterEach } from 'vitest';
import { Editor } from '@tiptap/core';
import StarterKit from '@tiptap/starter-kit';
import { BlockDragHandle } from './block-drag-handle';
import { acquire, __resetViewerBackdropForTests } from '$lib/a11y/viewerBackdrop';

/**
 * BUG-2453 — the block-drag global listeners defer to a frontmost viewer.
 *
 * TASK-2430 taught seven global owners to stand down for a frontmost
 * attachment viewer and DEFERRED two: svelte-dnd-action and this plugin's
 * editor block-drag gestures. This is that deferral.
 *
 * The state is not theoretical. Background inertness stops NEW interaction with
 * the editor; it does not stop a gesture that already owns the pointer. So a
 * block drag in flight when the viewer opens kept running against an `inert`,
 * invisible editor — and the drop still landed.
 *
 * FOUR owners, not the two the bug names: the mouse pair (`mousemove` /
 * `mouseup`) and the touch pair (`touchmove` / `touchend`) are installed on the
 * same line and had the identical hole.
 *
 * ACCEPTANCE IS PER-OWNER, per that task's contract: each owner proven inert
 * while a viewer is frontmost, AND each with an empty-stack regression.
 *
 * WHAT THE EMPTY-STACK LEGS PIN, stated because that task shipped and reverted
 * two tests that got this wrong: they assert TODAY'S behaviour, unchanged — not
 * behaviour this change would prefer. `isBlockedByModal` returns false on an
 * empty lease stack, so with no viewer every path below is untouched, and a leg
 * that failed here would mean the guard had leaked into the ordinary case.
 *
 * Driven through a real Tiptap editor rather than mocks, like its sibling
 * `blockDragHandleFreeze.svelte.test.ts`, because the handle and menu are
 * imperative DOM the plugin appends. The SELECTION path arms the handle;
 * the hover path needs `posAtCoords`, which jsdom lacks.
 */

function makeEditor(element: HTMLElement): Editor {
	return new Editor({
		element,
		extensions: [StarterKit, BlockDragHandle],
		content: '<p>alpha</p><p>bravo</p>',
		editable: true,
	});
}

function armHandle(editor: Editor, wrapper: HTMLElement, at: number): HTMLElement {
	wrapper.dispatchEvent(new MouseEvent('click', { bubbles: true }));
	editor.commands.setTextSelection(at);
	return wrapper.querySelector('.block-drag-handle') as HTMLElement;
}

/** Inside "alpha" (block 1) and inside "bravo" (block 2). */
const IN_FIRST_BLOCK = 3;
const IN_SECOND_BLOCK = 10;

/**
 * WHICH BLOCK A REORDER LEG MUST DRAG, and why it is not arbitrary.
 *
 * jsdom gives every element a zero `getBoundingClientRect`, so `dropPosAtY`
 * scores every candidate identically and keeps the FIRST it considers. For a
 * drag of block 1 that is the position immediately AFTER block 1 — a move to
 * where the node already is, which `executeMove` performs as a no-op. The drop
 * genuinely runs and changes nothing, so a doc-equality assertion passes
 * whether or not the guard fired.
 *
 * Dragging block 2 inverts it: the first candidate is position 0, ahead of
 * block 1, so the paragraphs swap and the assertion has something to see.
 * Measured both ways before this leg was written.
 */

/** A body-portaled viewer root, exactly as `Lightbox` mounts one. */
function mountViewer(): HTMLElement {
	const root = document.createElement('div');
	root.className = 'attachment-viewer';
	root.setAttribute('role', 'dialog');
	document.body.appendChild(root);
	return root;
}

function touchEvent(type: string, clientY: number): TouchEvent {
	const e = new Event(type, { bubbles: true, cancelable: true }) as unknown as {
		touches: { clientY: number }[];
	} & TouchEvent;
	e.touches = [{ clientY }];
	return e;
}

describe('BUG-2453 — block-drag global listeners defer to a frontmost viewer', () => {
	let editor: Editor | null = null;
	let element: HTMLElement | null = null;

	afterEach(() => {
		editor?.destroy();
		element?.remove();
		editor = null;
		element = null;
		document.querySelectorAll('.attachment-viewer').forEach((n) => n.remove());
		__resetViewerBackdropForTests();
	});

	function setup(at: number = IN_FIRST_BLOCK) {
		element = document.body.appendChild(document.createElement('div'));
		editor = makeEditor(element);
		const handle = armHandle(editor, element, at);
		return { editor, wrapper: element, handle };
	}

	/** Press the handle and move past the 5px threshold — a drag is now in flight. */
	function startMouseDrag(handle: HTMLElement) {
		handle.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, clientY: 100 }));
		window.dispatchEvent(new MouseEvent('mousemove', { bubbles: true, clientY: 130 }));
	}

	/**
	 * The observable signature of a drag in flight, taken from `startDrag`'s own
	 * writes. Deliberately NOT a ghost selector: the ghost is an UNCLASSED
	 * `cloneNode` of the block styled by inline `cssText`, so it matches no
	 * class at all — an earlier draft of this file asserted `.block-drag-ghost`
	 * and that selector was null in every state, which made this predicate
	 * always false and the teardown assertions vacuously true.
	 */
	function dragIsInFlight(editor: Editor, handle: HTMLElement): boolean {
		return (
			handle.classList.contains('active') &&
			editor.view.dom.style.pointerEvents === 'none' &&
			firstBlockDom(editor).style.opacity === '0.2'
		);
	}

	/** The paragraph `startDrag` dims to 0.2 and every teardown path restores. */
	function firstBlockDom(editor: Editor): HTMLElement {
		return editor.view.dom.querySelector('p') as HTMLElement;
	}

	/** The block `startDrag` dimmed — the one whose handle was armed. */
	function draggedBlockDom(editor: Editor, nth: number): HTMLElement {
		return editor.view.dom.querySelectorAll('p')[nth] as HTMLElement;
	}

	/** Paragraph text in document order — the observable a reorder changes. */
	function blockText(editor: Editor): string[] {
		return [...editor.view.dom.querySelectorAll('p')].map((n) => n.textContent ?? '');
	}

	/** The menu is built once at plugin init and toggled by `display`. */
	function menuIsOpen(): boolean {
		const menu = document.querySelector('.block-context-menu') as HTMLElement | null;
		return !!menu && menu.style.display !== 'none';
	}

	describe('mousemove — the straddling drag', () => {
		it('aborts the in-flight drag when a viewer opens, leaving no half-drag behind', () => {
			const { editor, handle } = setup();
			startMouseDrag(handle);
			// PRECONDITION: without this the rest proves nothing — a test that
			// never started a drag would pass against the unfixed code.
			expect(dragIsInFlight(editor, handle)).toBe(true);
			const before = JSON.stringify(editor.getJSON());

			const bodyChildrenDuringDrag = document.body.childElementCount;

			acquire(mountViewer());
			window.dispatchEvent(new MouseEvent('mousemove', { bubbles: true, clientY: 400 }));

			// Not merely "stopped" — torn down. An early return would leave every
			// one of these latched.
			expect(handle.classList.contains('active')).toBe(false);
			expect(editor.view.dom.style.pointerEvents).toBe('');
			expect(firstBlockDom(editor).style.opacity).toBe('');
			// The ghost is removed: one fewer body child than mid-drag, allowing
			// for the viewer root this leg just mounted.
			expect(document.body.childElementCount).toBe(bodyChildrenDuringDrag);
			expect(JSON.stringify(editor.getJSON())).toBe(before);
		});

		it('EMPTY STACK: a drag proceeds exactly as before when no viewer exists', () => {
			const { editor, handle } = setup();
			startMouseDrag(handle);
			expect(dragIsInFlight(editor, handle)).toBe(true);

			window.dispatchEvent(new MouseEvent('mousemove', { bubbles: true, clientY: 400 }));

			expect(dragIsInFlight(editor, handle)).toBe(true);
		});
	});

	describe('mouseup — the drop, and the menu branch', () => {
		it('does not commit the reorder when the mouseup lands under a viewer', () => {
			const { editor, handle } = setup(IN_SECOND_BLOCK);
			startMouseDrag(handle);
			// PRECONDITION: a drag really is in flight on the SECOND block.
			expect(draggedBlockDom(editor, 1).style.opacity).toBe('0.2');

			acquire(mountViewer());
			window.dispatchEvent(new MouseEvent('mouseup', { bubbles: true, clientY: 400 }));

			// The defect this bug was filed for: the drop landed in a document
			// the user could not see.
			expect(blockText(editor)).toEqual(['alpha', 'bravo']);
			expect(editor.view.dom.style.pointerEvents).toBe('');
		});

		it('EMPTY STACK: the same drag DOES reorder when no viewer exists', () => {
			const { editor, handle } = setup(IN_SECOND_BLOCK);
			startMouseDrag(handle);

			window.dispatchEvent(new MouseEvent('mouseup', { bubbles: true, clientY: 400 }));

			// The positive half. Without this the leg above could pass because
			// the drag never worked rather than because the guard stopped it.
			expect(blockText(editor)).toEqual(['bravo', 'alpha']);
		});

		it('does not open the block menu on a press that never moved', () => {
			const { handle } = setup();
			handle.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, clientY: 100 }));

			acquire(mountViewer());
			window.dispatchEvent(new MouseEvent('mouseup', { bubbles: true, clientY: 100 }));

			// The non-drag branch of this handler. A guard placed inside the
			// `dragging` arm would leave this menu open over the viewer.
			expect(menuIsOpen()).toBe(false);
		});

		it('EMPTY STACK: a press that never moved still opens the menu', () => {
			const { handle } = setup();
			handle.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, clientY: 100 }));
			window.dispatchEvent(new MouseEvent('mouseup', { bubbles: true, clientY: 100 }));

			expect(menuIsOpen()).toBe(true);
		});
	});

	describe('touchmove / touchend — the same hole on every touch device', () => {
		it('does not begin a drag from a pending touch while a viewer is frontmost', () => {
			const { editor, handle } = setup();
			handle.dispatchEvent(touchEvent('touchstart', 100));

			acquire(mountViewer());
			window.dispatchEvent(touchEvent('touchmove', 140));

			expect(handle.classList.contains('active')).toBe(false);
			expect(editor.view.dom.style.pointerEvents).toBe('');
			expect(firstBlockDom(editor).style.opacity).toBe('');
		});

		it('does not swallow the touch it declines to handle', () => {
			const { handle } = setup();
			handle.dispatchEvent(touchEvent('touchstart', 100));

			acquire(mountViewer());
			const move = touchEvent('touchmove', 140);
			window.dispatchEvent(move);

			// The touch belongs to the viewer. Calling preventDefault here would
			// break scrolling inside the thing the user is actually looking at.
			expect(move.defaultPrevented).toBe(false);
		});

		it('does not open the block menu on a tap under a viewer', () => {
			const { handle } = setup();
			handle.dispatchEvent(touchEvent('touchstart', 100));

			acquire(mountViewer());
			window.dispatchEvent(touchEvent('touchend', 100));

			expect(menuIsOpen()).toBe(false);
		});

		it('does not commit the reorder when the touchend lands under a viewer', () => {
			const { editor, handle } = setup(IN_SECOND_BLOCK);
			handle.dispatchEvent(touchEvent('touchstart', 100));
			window.dispatchEvent(touchEvent('touchmove', 140));
			expect(draggedBlockDom(editor, 1).style.opacity).toBe('0.2');

			acquire(mountViewer());
			window.dispatchEvent(touchEvent('touchend', 400));

			expect(blockText(editor)).toEqual(['alpha', 'bravo']);
		});

		it('EMPTY STACK: the same touch drag DOES reorder', () => {
			const { editor, handle } = setup(IN_SECOND_BLOCK);
			handle.dispatchEvent(touchEvent('touchstart', 100));
			window.dispatchEvent(touchEvent('touchmove', 140));

			window.dispatchEvent(touchEvent('touchend', 400));

			expect(blockText(editor)).toEqual(['bravo', 'alpha']);
		});

		it('EMPTY STACK: a tap on the handle still opens the menu', () => {
			const { handle } = setup();
			handle.dispatchEvent(touchEvent('touchstart', 100));
			window.dispatchEvent(touchEvent('touchend', 100));

			expect(menuIsOpen()).toBe(true);
		});
	});
});
