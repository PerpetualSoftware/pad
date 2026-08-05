// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// TASK-2430 — the split-pane divider is an app-shell resize surface with BOTH a
// pointer-captured drag and an arrow-key nudge. Both must defer to a frontmost
// viewer; the drag additionally has to survive the straddle case, since it
// holds the pointer capture from the moment it engages.
//
// Each blocked case is paired with an EMPTY-STACK REGRESSION.
//
// HOW TO READ THE PAIRS. The BLOCKED tests are what fail if a guard is deleted
// or weakened. The EMPTY-STACK REGRESSIONS are the opposite check — they fail
// if a guard declines UNCONDITIONALLY, which is the way a "deference" change
// silently breaks the app for the 99% of the time no viewer is open. Neither
// half subsumes the other, and an empty-stack test passing with the guard
// removed is by design, not a false green. Every guard in the files under test
// was mutation-verified to kill at least one case here (one documented
// exception, flagged at the guard itself).
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick, flushSync } from 'svelte';

// ItemDetail (mounted inside the pane) is a large data-driven component; it is
// irrelevant to the divider, so stub the whole API surface it reaches for.
vi.mock('$lib/api/client', () => {
	const never = () => new Promise(() => {});
	return {
		api: new Proxy(
			{},
			{
				get: () =>
					new Proxy(
						{},
						{
							get: () => never,
						},
					),
			},
		),
		isPlanLimitError: () => false,
		planLimitMessage: () => '',
	};
});

import PaneHost from './PaneHost.svelte';
import { acquire, __resetViewerBackdropForTests } from '$lib/a11y/viewerBackdrop';

function divider(): HTMLElement {
	const el = document.querySelector('.pane-divider');
	if (!el) throw new Error('.pane-divider not found');
	return el as HTMLElement;
}

function paneWidth(): number {
	return Number(divider().getAttribute('aria-valuenow'));
}

function mountViewer(): HTMLElement {
	const root = document.createElement('div');
	root.className = 'attachment-viewer';
	root.setAttribute('role', 'dialog');
	document.body.appendChild(root);
	return root;
}

function pointer(type: string, x: number): Event {
	const e = new MouseEvent(type, { bubbles: true, cancelable: true, clientX: x, clientY: 0 });
	Object.defineProperty(e, 'pointerId', { value: 3 });
	Object.defineProperty(e, 'button', { value: 0 });
	return e;
}

function arrow(k: 'ArrowLeft' | 'ArrowRight'): KeyboardEvent {
	return new KeyboardEvent('keydown', { key: k, bubbles: true, cancelable: true });
}

let captured: number[] = [];
let released: number[] = [];
/**
 * `vi.restoreAllMocks()` does NOT undo a raw prototype assignment, and these
 * three are raw (jsdom implements neither pointer-capture method, and the rect
 * stub has to be a plain function to branch on the element). Keep the originals
 * and put them back, so nothing here can leak into another test in this file —
 * or, if vitest is ever run with `isolate: false`, into another file.
 */
/**
 * Put a prototype method back EXACTLY as it was. jsdom ships neither
 * pointer-capture method, so assigning the captured `undefined` would CREATE an
 * own property and flip `in` / `hasOwn` feature detection for later tests —
 * delete instead.
 */
function restoreProto(name: string, original: unknown) {
	if (original === undefined) {
		delete (Element.prototype as unknown as Record<string, unknown>)[name];
	} else {
		(Element.prototype as unknown as Record<string, unknown>)[name] = original;
	}
}

const REAL = {
	setPointerCapture: Element.prototype.setPointerCapture,
	releasePointerCapture: Element.prototype.releasePointerCapture,
	getBoundingClientRect: Element.prototype.getBoundingClientRect,
};

beforeEach(async () => {
	captured = [];
	released = [];
	(Element.prototype as unknown as Record<string, unknown>).setPointerCapture = function (
		id: number,
	) {
		captured.push(id);
	};
	(Element.prototype as unknown as Record<string, unknown>).releasePointerCapture = function (
		id: number,
	) {
		released.push(id);
	};
	// jsdom reports a zero-size rect, and the component's fit/clamp bounds are
	// derived from the CONTAINER width — with everything zero, every width pins
	// to the same clamped value and no assertion here could distinguish a resize
	// from a no-op.
	//
	// So describe ONE COHERENT LAYOUT rather than convenient numbers: a 2000px
	// container spanning [0, 2000], with the right-docked pane occupying its
	// right edge — [1600, 2000], i.e. 400 wide, its `right` equal to the
	// container's. The divider therefore sits at x≈1600, and the drag math
	// (`pane.right - clientX`) reads as it does in a browser: dragging to 1400
	// gives a 600px pane, and the list keeps 1400px.
	Element.prototype.getBoundingClientRect = function (this: Element) {
		if (this.classList?.contains('item-pane')) {
			return { right: 2000, left: 1600, top: 0, bottom: 800, width: 400, height: 800, x: 1600, y: 0 } as DOMRect;
		}
		return { right: 2000, left: 0, top: 0, bottom: 800, width: 2000, height: 800, x: 0, y: 0 } as DOMRect;
	};
	localStorage.removeItem('pad-pane-width');

	render(PaneHost, {
		props: {
			openItemRef: 'TASK-1',
			username: 'u',
			wsSlug: 'ws',
			collSlug: 'tasks',
			paneMintForRoute: 'TASK-1',
			onClose: vi.fn(),
			onGone: vi.fn(),
			onNavigateAway: vi.fn(),
			onOpenTarget: vi.fn(),
			onBack: vi.fn(),
		},
	});
	await tick();
	flushSync();
});

afterEach(() => {
	cleanup();
	__resetViewerBackdropForTests();
	restoreProto('setPointerCapture', REAL.setPointerCapture);
	restoreProto('releasePointerCapture', REAL.releasePointerCapture);
	restoreProto('getBoundingClientRect', REAL.getBoundingClientRect);
	// The component PERSISTS the width it computes; without this the next test's
	// baseline is the previous test's drag result.
	localStorage.removeItem('pad-pane-width');
	vi.restoreAllMocks();
	document.body.innerHTML = '';
});

describe('PaneHost divider — keyboard resize (TASK-2430)', () => {
	it('EMPTY-STACK REGRESSION: an arrow still nudges the width with no lease held', async () => {
		const before = paneWidth();
		const e = arrow('ArrowLeft');
		divider().dispatchEvent(e);
		await tick();
		flushSync();
		expect(e.defaultPrevented).toBe(true);
		expect(paneWidth()).toBeGreaterThan(before);
	});

	it('ignores arrows while a viewer lease is frontmost, and resumes on release', async () => {
		const lease = acquire(mountViewer());
		const before = paneWidth();
		const blocked = arrow('ArrowLeft');
		divider().dispatchEvent(blocked);
		await tick();
		flushSync();
		expect(blocked.defaultPrevented).toBe(false);
		expect(paneWidth()).toBe(before);

		lease.release();
		const after = arrow('ArrowLeft');
		divider().dispatchEvent(after);
		await tick();
		flushSync();
		expect(after.defaultPrevented).toBe(true);
		expect(paneWidth()).toBeGreaterThan(before);
	});
});

describe('PaneHost divider — drag resize (TASK-2430)', () => {
	it('EMPTY-STACK REGRESSION: a drag still engages, captures and resizes', async () => {
		const d = divider();
		const down = pointer('pointerdown', 1600);
		d.dispatchEvent(down);
		expect(down.defaultPrevented).toBe(true);
		expect(captured).toEqual([3]);

		d.dispatchEvent(pointer('pointermove', 1400));
		await tick();
		flushSync();
		// `pane.right (2000) - clientX (1400)` = 600, inside the clamp bounds for
		// a 2000px container.
		expect(paneWidth()).toBe(600);
	});

	it('does not START a drag while a viewer lease is frontmost', async () => {
		acquire(mountViewer());
		const before = paneWidth();
		const d = divider();
		const down = pointer('pointerdown', 1600);
		d.dispatchEvent(down);
		expect(down.defaultPrevented).toBe(false);
		expect(captured).toEqual([]);

		d.dispatchEvent(pointer('pointermove', 1400));
		await tick();
		flushSync();
		expect(paneWidth()).toBe(before);
		// The global drag chrome was never installed either.
		expect(document.body.style.cursor).toBe('');
	});

	it('STRADDLE: a drag in flight is ENDED when the viewer opens, not merely paused', async () => {
		const d = divider();
		d.dispatchEvent(pointer('pointerdown', 1600));
		d.dispatchEvent(pointer('pointermove', 1400));
		await tick();
		flushSync();
		expect(paneWidth()).toBe(600);
		expect(document.body.style.cursor).toBe('col-resize');

		acquire(mountViewer());
		d.dispatchEvent(pointer('pointermove', 1300));
		await tick();
		flushSync();
		// Width unchanged, capture released, and the global drag chrome restored
		// — an early `return` alone would have left all three wrong.
		expect(paneWidth()).toBe(600);
		expect(released).toEqual([3]);
		expect(document.body.style.cursor).toBe('');
		expect(document.body.style.userSelect).toBe('');

		// The drag is genuinely over: a further move does nothing even if the
		// viewer were to close.
		d.dispatchEvent(pointer('pointermove', 1250));
		await tick();
		flushSync();
		expect(paneWidth()).toBe(600);
	});
});
