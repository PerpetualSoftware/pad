import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { mount, unmount, flushSync } from 'svelte';
import type { Snapshot } from '@sveltejs/kit';
import RestoreHarness from './fixtures/RestoreHarness.svelte';
import {
	acquire,
	__resetViewerBackdropForTests,
	VIEWER_ROOT_CLASS,
} from '$lib/a11y/viewerBackdrop';
import { isModalViewerScrollInput } from './restore.svelte';

// TASK-2457 — a page scroll-restoration must not be aborted by input the user
// aimed at the frontmost attachment viewer (its own wheel-zoom / arrow-nav),
// nor by any event a modal already handled (`defaultPrevented`). A GENUINE
// non-viewer scroll still aborts, exactly as before.
//
// The restoration installs passive window listeners (wheel / touchmove /
// keydown) during a restore and bails the moment one fires. These tests drive a
// real restore to the point those listeners are live, dispatch an event, and
// observe whether the restore PROCEEDED (its `scrollTo` ran) or ABORTED.

let rafQueue: FrameRequestCallback[] = [];
const mounted: ReturnType<typeof mount>[] = [];
let appRoot: HTMLElement;

function frame(): void {
	const cb = rafQueue.shift();
	if (cb) cb(performance.now());
}

beforeEach(() => {
	rafQueue = [];
	// Drive requestAnimationFrame by hand so the restore loop is deterministic.
	vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => {
		rafQueue.push(cb);
		return rafQueue.length;
	});
	appRoot = document.body.appendChild(document.createElement('div'));
});

afterEach(() => {
	while (mounted.length) unmount(mounted.pop()!);
	// Drain any queued restore frames so their window wheel/touch/key listeners
	// self-remove now that the harness is unmounted (alive === false → the loop
	// calls cleanupListeners). A "proceeded" restore never reaches its completion
	// frame here (the fake target's scrollTop never advances), so without this the
	// listeners would leak across tests — harmless (they mutate dead closures) but
	// untidy.
	while (rafQueue.length) frame();
	document.body.innerHTML = '';
	rafQueue = [];
	__resetViewerBackdropForTests();
	vi.unstubAllGlobals();
	vi.restoreAllMocks();
});

/** A stand-in scroll container; `scrollTo` firing is the "restore proceeded" signal. */
function makeTarget() {
	return {
		scrollTop: 0,
		scrollHeight: 5000,
		scrollTo: vi.fn(),
	} as unknown as HTMLElement;
}

/**
 * Mount the harness, seed a pending restore so the input listeners go live, run
 * `dispatch`, then advance to the first scroll attempt. Returns whether the
 * restore PROCEEDED (its `scrollTo` ran) — i.e. the dispatched input was NOT
 * treated as the user scrolling the page.
 */
function driveRestore(dispatch: () => void): boolean {
	const target = makeTarget();
	let snap: Snapshot<number> | undefined;
	const app = mount(RestoreHarness, {
		target: appRoot,
		props: {
			ready: () => true,
			scrollTarget: () => target,
			expose: (s: Snapshot<number>) => (snap = s),
		},
	});
	mounted.push(app);
	flushSync();

	// Seed a pending restore: the gated effect registers the wheel / touch / key
	// listeners on `window` and schedules the double-RAF scroll loop.
	snap!.restore!(100);
	flushSync();

	dispatch();

	// Double-RAF, then the first `tryScroll`: it bails at the top when the user
	// scrolled, otherwise it calls `scrollTo`.
	frame();
	frame();
	return (target.scrollTo as unknown as ReturnType<typeof vi.fn>).mock.calls.length > 0;
}

/**
 * A body-attached element carrying the viewer class, made the frontmost lease.
 * Attached to `<body>` directly, as the real viewer portals itself, so the
 * backdrop manager's inert bookkeeping has the body child it expects.
 */
function frontmostViewer(): HTMLElement {
	const v = document.body.appendChild(document.createElement('div'));
	v.classList.add(VIEWER_ROOT_CLASS);
	acquire(v);
	return v;
}

function plain(): HTMLElement {
	return appRoot.appendChild(document.createElement('div'));
}

describe('scroll restoration — ignores frontmost-viewer and handled input (TASK-2457)', () => {
	it('SANITY: with no interfering input, a pending restore proceeds to scrollTo', () => {
		// Proves the harness actually reaches the scroll — without this, every
		// "proceeded" assertion below could be a false green.
		expect(driveRestore(() => {})).toBe(true);
	});

	// ── wheel ──
	it('a WHEEL originating in the frontmost viewer does NOT abort the restore', () => {
		expect(
			driveRestore(() => frontmostViewer().dispatchEvent(new WheelEvent('wheel', { bubbles: true })))
		).toBe(true);
	});

	it('CONTROL: a genuine non-viewer WHEEL aborts the restore', () => {
		expect(
			driveRestore(() => plain().dispatchEvent(new WheelEvent('wheel', { bubbles: true })))
		).toBe(false);
	});

	// ── key (the viewer's arrow-nav, shipped 3a) ──
	it('an ARROW key originating in the frontmost viewer does NOT abort the restore', () => {
		expect(
			driveRestore(() =>
				frontmostViewer().dispatchEvent(
					new KeyboardEvent('keydown', { key: 'ArrowLeft', bubbles: true })
				)
			)
		).toBe(true);
	});

	it('a defaultPrevented ARROW key (handled by a modal) does NOT abort, even from outside a viewer', () => {
		// The `defaultPrevented` branch, independent of origin — the viewer
		// preventDefaults its arrows, so the next modal inherits the fix.
		expect(
			driveRestore(() => {
				const e = new KeyboardEvent('keydown', {
					key: 'ArrowLeft',
					bubbles: true,
					cancelable: true,
				});
				e.preventDefault();
				plain().dispatchEvent(e);
			})
		).toBe(true);
	});

	it('CONTROL: a genuine non-viewer scroll key aborts the restore', () => {
		expect(
			driveRestore(() =>
				plain().dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }))
			)
		).toBe(false);
	});

	// ── touch (the viewer owns touch via POINTER handlers since 3d, but a touchmove is
	//    still NOT defaultPrevented — only the origin check catches it) ──
	it('a TOUCHMOVE originating in the frontmost viewer does NOT abort the restore', () => {
		expect(
			driveRestore(() => frontmostViewer().dispatchEvent(new Event('touchmove', { bubbles: true })))
		).toBe(true);
	});

	it('CONTROL: a genuine non-viewer TOUCHMOVE aborts the restore', () => {
		expect(
			driveRestore(() => plain().dispatchEvent(new Event('touchmove', { bubbles: true })))
		).toBe(false);
	});
});

describe('isModalViewerScrollInput (TASK-2457)', () => {
	it('true for a defaultPrevented event, whatever its target', () => {
		const e = new WheelEvent('wheel', { cancelable: true });
		e.preventDefault();
		expect(isModalViewerScrollInput(e)).toBe(true);
	});

	it('true for an event originating inside the FRONTMOST viewer', () => {
		const v = frontmostViewer();
		const child = v.appendChild(document.createElement('button'));
		expect(isModalViewerScrollInput({ defaultPrevented: false, target: child } as unknown as Event)).toBe(
			true
		);
	});

	it('false for an event inside a viewer that is NOT frontmost', () => {
		const back = frontmostViewer(); // acquired first
		frontmostViewer(); // a second viewer is now frontmost
		expect(
			isModalViewerScrollInput({ defaultPrevented: false, target: back } as unknown as Event)
		).toBe(false);
	});

	it('false for an event outside any viewer', () => {
		expect(
			isModalViewerScrollInput({ defaultPrevented: false, target: plain() } as unknown as Event)
		).toBe(false);
	});

	it('false for a non-Element target', () => {
		expect(
			isModalViewerScrollInput({ defaultPrevented: false, target: window } as unknown as Event)
		).toBe(false);
	});
});
