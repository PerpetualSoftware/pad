// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// TASK-2430 — the per-item dependency graph CO-MOUNTS with the attachment
// viewer: the drawer stays in the DOM while a viewer opens over it. Its
// pan/zoom must therefore defer to a frontmost viewer.
//
// The straddle cases matter more here than anywhere else in the app: this
// viewport has NO `lostpointercapture` handler and calls `setPointerCapture`
// once a real drag engages, so a pan that begins before the viewer opens keeps
// receiving moves afterwards and CANNOT be stopped by a start-gate alone.
//
// Every blocked case is paired with an EMPTY-STACK REGRESSION proving the
// gesture is untouched with no lease held. Every guard in ItemGraph.svelte is
// mutation-verified to kill at least one case below — there is no guard here
// that a test cannot fail on, because the one that would have been (pointerup)
// was deleted rather than kept as decoration.
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

vi.mock('$lib/api/client', () => ({
	api: {
		graph: {
			// A single focused node is enough to reach `loadState === 'ready'`,
			// which is what renders the pannable <g transform=…> the pan
			// assertions read.
			getFocused: vi.fn(async () => ({
				nodes: [
					{
						id: 'id-1',
						ref: 'TASK-1',
						title: 'Focus',
						collection: 'tasks',
						status: 'todo',
						is_terminal: false,
						child_count: 0,
						updated_at: '2026-01-01T00:00:00Z',
					},
				],
				edges: [],
			})),
		},
	},
}));
vi.mock('$lib/services/sse.svelte', () => ({
	sseService: {
		onItemEvent: () => () => {},
		onSyncRequired: () => () => {},
	},
}));

import ItemGraph from './ItemGraph.svelte';
import { acquire, __resetViewerBackdropForTests } from '$lib/a11y/viewerBackdrop';

function baseProps(overrides: Record<string, unknown> = {}) {
	return {
		workspace: 'ws',
		focusRef: 'TASK-1',
		itemHref: () => '/x',
		onOpenTarget: vi.fn(),
		...overrides,
	};
}

function viewport(): HTMLElement {
	const el = document.querySelector('.viewport');
	if (!el) throw new Error('.viewport not found');
	return el as HTMLElement;
}

/** The pannable content transform — the observable proof a pan moved anything. */
function transform(): string {
	const el = document.querySelector('.viewport svg g[transform^="translate"]');
	return el?.getAttribute('transform') ?? '';
}

function mountViewer(): HTMLElement {
	const root = document.createElement('div');
	root.className = 'attachment-viewer';
	root.setAttribute('role', 'dialog');
	document.body.appendChild(root);
	return root;
}

/** jsdom has no PointerEvent; MouseEvent + the pointer fields the code reads. */
function pointer(type: string, x: number, y: number, buttons = 1): Event {
	const e = new MouseEvent(type, { bubbles: true, cancelable: true, clientX: x, clientY: y });
	Object.defineProperty(e, 'pointerId', { value: 7 });
	Object.defineProperty(e, 'buttons', { value: buttons });
	Object.defineProperty(e, 'button', { value: 0 });
	return e;
}

let captured: number[] = [];
let released: number[] = [];
/** Raw prototype assignments below; `vi.restoreAllMocks()` cannot undo those. */
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
};

beforeEach(() => {
	captured = [];
	released = [];
	// jsdom implements neither; record the calls so the straddle tests can
	// assert the capture is actually RELEASED, not merely ignored.
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
});

afterEach(() => {
	cleanup();
	__resetViewerBackdropForTests();
	restoreProto('setPointerCapture', REAL.setPointerCapture);
	restoreProto('releasePointerCapture', REAL.releasePointerCapture);
	vi.restoreAllMocks();
	document.body.innerHTML = '';
});

async function mount() {
	render(ItemGraph, { props: baseProps() });
	// Mount kicks off the fetch; the resolved payload then lays out with dagre.
	// Poll rather than count ticks — the chain is async and layout-dependent.
	for (let i = 0; i < 50 && !document.querySelector('.viewport svg .node'); i++) {
		await new Promise((r) => setTimeout(r, 5));
		flushSync();
	}
	// FAIL LOUDLY rather than fall through. Every blocked-gesture assertion below
	// is negative ("nothing moved"), so a viewport still stuck in `loading` — no
	// pannable <g>, no nodes — would satisfy all of them vacuously. Assert the
	// component actually reached its ready state before any test runs.
	expect(document.querySelector('.viewport svg .node')).not.toBeNull();
	expect(document.querySelector('.viewport .state-overlay')).toBeNull();
	expect(transform()).toMatch(/^translate\(/);
	return viewport();
}

describe('ItemGraph — wheel zoom (TASK-2430)', () => {
	it('EMPTY-STACK REGRESSION: consumes the wheel and zooms with no lease held', async () => {
		const vp = await mount();
		const e = new WheelEvent('wheel', { bubbles: true, cancelable: true, deltaY: -100 });
		vp.dispatchEvent(e);
		// `preventDefault()` is the first thing the un-blocked path does, so it is
		// the cleanest observable signal that the handler ran.
		expect(e.defaultPrevented).toBe(true);
	});

	it('leaves the wheel alone while a viewer lease is frontmost, and takes it back on release', async () => {
		const vp = await mount();
		const lease = acquire(mountViewer());

		const blocked = new WheelEvent('wheel', { bubbles: true, cancelable: true, deltaY: -100 });
		vp.dispatchEvent(blocked);
		expect(blocked.defaultPrevented).toBe(false);

		lease.release();
		const after = new WheelEvent('wheel', { bubbles: true, cancelable: true, deltaY: -100 });
		vp.dispatchEvent(after);
		expect(after.defaultPrevented).toBe(true);
	});
});

describe('ItemGraph — pan (TASK-2430)', () => {
	it('EMPTY-STACK REGRESSION: a drag still engages and captures the pointer', async () => {
		const vp = await mount();
		const baseline = transform();
		vp.dispatchEvent(pointer('pointerdown', 100, 100));
		vp.dispatchEvent(pointer('pointermove', 200, 200));
		flushSync();
		expect(captured).toEqual([7]);
		expect(transform()).not.toBe(baseline);

		vp.dispatchEvent(pointer('pointerup', 200, 200, 0));
		expect(released).toEqual([7]);
	});

	it('does not START a pan while a viewer lease is frontmost', async () => {
		const vp = await mount();
		const baseline = transform();
		acquire(mountViewer());

		vp.dispatchEvent(pointer('pointerdown', 100, 100));
		vp.dispatchEvent(pointer('pointermove', 200, 200));
		flushSync();
		expect(captured).toEqual([]);
		expect(transform()).toBe(baseline);
	});

	it('a press made under a viewer does not become a pan when the viewer closes', async () => {
		// The move gate alone would satisfy the "does not START" test above — it
		// aborts on the next event either way. This is what makes the START gate
		// itself load-bearing: the press never armed the drag, so nothing resumes.
		const vp = await mount();
		const baseline = transform();
		const lease = acquire(mountViewer());
		vp.dispatchEvent(pointer('pointerdown', 100, 100));

		lease.release();
		vp.dispatchEvent(pointer('pointermove', 300, 300));
		flushSync();
		expect(captured).toEqual([]);
		expect(transform()).toBe(baseline);
	});

	it('STRADDLE: a pan already holding the pointer capture is torn down when the viewer opens', async () => {
		const vp = await mount();
		const baseline = transform();
		// Engage a real pan first — capture is now held by the viewport.
		vp.dispatchEvent(pointer('pointerdown', 100, 100));
		vp.dispatchEvent(pointer('pointermove', 150, 150));
		flushSync();
		expect(captured).toEqual([7]);
		const engaged = transform();
		expect(engaged).not.toBe(baseline);

		// The viewer opens mid-gesture.
		acquire(mountViewer());
		vp.dispatchEvent(pointer('pointermove', 400, 400));
		flushSync();
		// The pan neither advanced…
		expect(transform()).toBe(engaged);
		// …nor kept the capture — which is the part an early `return` alone would
		// get wrong, leaving this canvas swallowing every pointer event on the
		// page while the viewer is up.
		expect(released).toEqual([7]);

		// And a further move changes nothing: the drag is genuinely over.
		vp.dispatchEvent(pointer('pointermove', 800, 800));
		flushSync();
		expect(transform()).toBe(engaged);
	});

	// There is deliberately NO arbitration gate on pointerup (see the note at
	// `onPointerUp` in ItemGraph.svelte — it would be unfalsifiable dead code).
	// This asserts the INVARIANT that must hold without one: a viewer opening
	// between the last move and the release still leaves nothing captured.
	it('a press whose pointerup RETARGETS to the viewer cannot resume a pan later', async () => {
		// The one sequence the move/end gates cannot see (review round 10): the
		// press never crosses DRAG_THRESHOLD, so no capture is taken; the viewer
		// then opens and the release lands on the PORTALED viewer, so this
		// viewport's `onPointerUp` never runs at all and `maybeDrag` stays
		// latched. Pre-existing — capture-less presses ending off-viewport have
		// always been possible — and the existing `(e.buttons & 1) === 0` abort in
		// `onPointerMove` is the mitigation. Asserted here so the deletion of the
		// pointerup gate cannot be blamed for it, and so the mitigation cannot be
		// removed silently.
		const vp = await mount();
		const baseline = transform();
		vp.dispatchEvent(pointer('pointerdown', 100, 100));

		// Viewer opens; the release goes to the viewer, never to the viewport.
		const viewer = mountViewer();
		const lease = acquire(viewer);
		viewer.dispatchEvent(pointer('pointerup', 120, 120, 0));
		lease.release();

		// A later move with NO button held must abort the stale press, not pan.
		vp.dispatchEvent(pointer('pointermove', 600, 600, 0));
		flushSync();
		expect(captured).toEqual([]);
		expect(transform()).toBe(baseline);

		// ...and the press is genuinely dead: even a button-held move does nothing
		// until a fresh pointerdown arrives.
		vp.dispatchEvent(pointer('pointermove', 700, 700));
		flushSync();
		expect(captured).toEqual([]);
		expect(transform()).toBe(baseline);
	});

	it('STRADDLE: a viewer opening between the last move and pointerup still releases the capture', async () => {
		const vp = await mount();
		vp.dispatchEvent(pointer('pointerdown', 100, 100));
		vp.dispatchEvent(pointer('pointermove', 150, 150));
		flushSync();
		expect(captured).toEqual([7]);

		acquire(mountViewer());
		vp.dispatchEvent(pointer('pointerup', 150, 150, 0));
		expect(released).toEqual([7]);
	});
});
