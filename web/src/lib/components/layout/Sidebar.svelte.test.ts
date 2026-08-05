// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// TASK-2430 — the sidebar owns two swipe gestures that are global surfaces: the
// CLOSE swipe on the `<aside>` itself, and the mobile left-edge OPEN swipe,
// which is a `<svelte:window>` touch handler firing anywhere on the page. Both
// must stand down while a viewer is frontmost, at gesture START **and** on the
// captured move/end — a touch sequence keeps being delivered to its original
// target after the viewer opens.
//
// Every blocked case is paired with an EMPTY-STACK REGRESSION.
//
// HOW TO READ THE PAIRS. The BLOCKED tests are what fail if a guard is deleted
// or weakened. The EMPTY-STACK REGRESSIONS are the opposite check — they fail
// if a guard declines UNCONDITIONALLY, which is the way a "deference" change
// silently breaks the app for the 99% of the time no viewer is open. Neither
// half subsumes the other, and an empty-stack test passing with the guard
// removed is by design, not a false green. Every guard in the files under test
// was mutation-verified to kill at least one case here (one documented
// exception, flagged at the guard itself).
import { describe, it, expect, vi, beforeEach, afterEach, afterAll } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick, flushSync } from 'svelte';

// Force the MOBILE viewport BEFORE any import is evaluated: `breakpoint.svelte.ts`
// reads `matchMedia` at MODULE LOAD, so a `beforeEach` stub is far too late and
// the edge-open swipe (gated on `uiStore.isMobile`) would be unreachable.
//
// ASSUMES PER-FILE MODULE ISOLATION — vitest's default, and what this repo runs
// (`npm run test`, no `--no-isolate`, no pool override, in the config or in CI).
// Under a shared module graph `breakpoint.svelte.ts` may already have been
// imported and its viewport flag resolved against the DESKTOP default before
// this hoisted stub runs, and this file's edge-swipe tests would fail. That is
// not a regression introduced here: the suite already has order-dependent
// failures under `--no-isolate` (verified on the base commit, in a different
// file), so the mode is unsupported today rather than newly broken. Storage
// leakage specifically is covered regardless — `setup-jsdom.ts` clears both
// Storages before every test, not per setup-file load.
const { realMatchMedia } = vi.hoisted(() => {
	const real = (globalThis as unknown as { matchMedia: unknown }).matchMedia;
	(globalThis as unknown as { matchMedia: unknown }).matchMedia = (query: string) => ({
		matches: true,
		media: query,
		onchange: null,
		addEventListener: () => {},
		removeEventListener: () => {},
		addListener: () => {},
		removeListener: () => {},
		dispatchEvent: () => false,
	});
	return { realMatchMedia: real };
});

// A raw assignment is NOT undone by `vi.restoreAllMocks()`, and `setup-jsdom.ts`
// only installs its desktop default when `matchMedia` is missing — so without
// this the mobile stub would survive this file and make any later file that
// reads the breakpoint at module load order-dependent.
afterAll(() => {
	(globalThis as unknown as { matchMedia: unknown }).matchMedia = realMatchMedia;
});

vi.mock('$lib/api/client', () => ({
	api: {
		health: vi.fn(async () => ({ version: 'dev', commit: 'abcdef0' })),
		collections: { list: vi.fn(async () => []) },
	},
	isPlanLimitError: () => false,
	planLimitMessage: () => '',
}));

import Sidebar from './Sidebar.svelte';
import { uiStore } from '$lib/stores/ui.svelte';
import { acquire, __resetViewerBackdropForTests } from '$lib/a11y/viewerBackdrop';

function aside(): HTMLElement {
	const el = document.querySelector('aside.sidebar');
	if (!el) throw new Error('aside.sidebar not found');
	return el as HTMLElement;
}

function mountViewer(): HTMLElement {
	const root = document.createElement('div');
	root.className = 'attachment-viewer';
	root.setAttribute('role', 'dialog');
	document.body.appendChild(root);
	return root;
}

/** jsdom has no TouchEvent constructor; a plain Event with `touches` suffices. */
function touch(type: string, clientX: number, clientY = 300, target?: HTMLElement): Event {
	const e = new Event(type, { bubbles: true, cancelable: true });
	Object.defineProperty(e, 'touches', { value: [{ clientX, clientY }] });
	if (target) Object.defineProperty(e, 'target', { value: target });
	return e;
}

beforeEach(async () => {
	uiStore.openSidebar();
	render(Sidebar, { props: {} });
	await tick();
	flushSync();
});

afterEach(() => {
	cleanup();
	__resetViewerBackdropForTests();
	vi.restoreAllMocks();
	document.body.innerHTML = '';
});

describe('Sidebar — swipe-to-close (TASK-2430)', () => {
	it('EMPTY-STACK REGRESSION: a past-threshold swipe still closes the sidebar', () => {
		expect(uiStore.sidebarOpen).toBe(true);
		const el = aside();
		el.dispatchEvent(touch('touchstart', 200));
		el.dispatchEvent(touch('touchmove', 40));
		flushSync();
		// The drawer visibly followed the finger — proof the gesture engaged.
		expect(el.style.transform).toBe('translateX(-160px)');
		el.dispatchEvent(touch('touchend', 40));
		expect(uiStore.sidebarOpen).toBe(false);
	});

	it('does not START a close swipe while a viewer lease is frontmost', () => {
		acquire(mountViewer());
		const el = aside();
		el.dispatchEvent(touch('touchstart', 200));
		el.dispatchEvent(touch('touchmove', 40));
		flushSync();
		expect(el.style.transform).toBe('');
		el.dispatchEvent(touch('touchend', 40));
		expect(uiStore.sidebarOpen).toBe(true);
	});

	it('STRADDLE: a close swipe begun before the viewer opened is abandoned mid-drag', () => {
		const el = aside();
		el.dispatchEvent(touch('touchstart', 200));
		el.dispatchEvent(touch('touchmove', 100));
		flushSync();
		expect(el.style.transform).toBe('translateX(-100px)');

		acquire(mountViewer());
		el.dispatchEvent(touch('touchmove', 20));
		flushSync();
		// The in-progress transform is UNDONE, not merely frozen.
		expect(el.style.transform).toBe('');

		el.dispatchEvent(touch('touchend', 20));
		expect(uiStore.sidebarOpen).toBe(true);
	});

	it('STRADDLE: a viewer opening between the last move and the release still blocks the close', () => {
		const el = aside();
		el.dispatchEvent(touch('touchstart', 200));
		el.dispatchEvent(touch('touchmove', 40));
		flushSync();
		// Already past the threshold and un-blocked to here, so only the gate on
		// the terminal event can stop the close.
		expect(el.style.transform).toBe('translateX(-160px)');

		acquire(mountViewer());
		el.dispatchEvent(touch('touchend', 40));
		expect(uiStore.sidebarOpen).toBe(true);
	});

	it('a gesture STARTED under a viewer does not come alive when the viewer closes', () => {
		// The move/end gates alone would pass the "does not START" test above —
		// they cancel the drag on the next event either way. This is what makes
		// the START gate itself load-bearing: with it, the press under the viewer
		// never armed the gesture, so nothing resumes once the lease is gone.
		const lease = acquire(mountViewer());
		const el = aside();
		el.dispatchEvent(touch('touchstart', 200));

		lease.release();
		el.dispatchEvent(touch('touchmove', 40));
		flushSync();
		expect(el.style.transform).toBe('');
		el.dispatchEvent(touch('touchend', 40));
		expect(uiStore.sidebarOpen).toBe(true);
	});

	it('takes the gesture back once the lease is released', () => {
		const lease = acquire(mountViewer());
		const el = aside();
		el.dispatchEvent(touch('touchstart', 200));
		el.dispatchEvent(touch('touchend', 40));
		expect(uiStore.sidebarOpen).toBe(true);

		lease.release();
		el.dispatchEvent(touch('touchstart', 200));
		el.dispatchEvent(touch('touchmove', 40));
		el.dispatchEvent(touch('touchend', 40));
		expect(uiStore.sidebarOpen).toBe(false);
	});
});

describe('Sidebar — mobile left-edge swipe-to-open (TASK-2430)', () => {
	// The window handler only arms when the sidebar is CLOSED on mobile.
	beforeEach(() => {
		uiStore.closeSidebar();
	});

	it('EMPTY-STACK REGRESSION: an edge swipe still opens the sidebar', () => {
		window.dispatchEvent(touch('touchstart', 4, 300, document.body));
		window.dispatchEvent(touch('touchmove', 40, 300, document.body));
		window.dispatchEvent(touch('touchmove', 120, 300, document.body));
		expect(uiStore.sidebarOpen).toBe(true);
	});

	it('does not START an edge swipe while a viewer lease is frontmost', () => {
		acquire(mountViewer());
		window.dispatchEvent(touch('touchstart', 4, 300, document.body));
		window.dispatchEvent(touch('touchmove', 40, 300, document.body));
		window.dispatchEvent(touch('touchmove', 120, 300, document.body));
		expect(uiStore.sidebarOpen).toBe(false);
	});

	it('an edge swipe STARTED under a viewer does not come alive when the viewer closes', () => {
		// Same point as the close-swipe case: proves the START gate, which the
		// move gate would otherwise mask.
		const lease = acquire(mountViewer());
		window.dispatchEvent(touch('touchstart', 4, 300, document.body));

		lease.release();
		window.dispatchEvent(touch('touchmove', 40, 300, document.body));
		window.dispatchEvent(touch('touchmove', 200, 300, document.body));
		expect(uiStore.sidebarOpen).toBe(false);
	});

	it('STRADDLE: an edge swipe begun before the viewer opened does not go on to open it', () => {
		// Arm and direction-lock the gesture with no viewer present…
		window.dispatchEvent(touch('touchstart', 4, 300, document.body));
		window.dispatchEvent(touch('touchmove', 40, 300, document.body));
		expect(uiStore.sidebarOpen).toBe(false); // locked, not yet far enough

		// …then the viewer opens before the swipe reaches its open distance.
		acquire(mountViewer());
		window.dispatchEvent(touch('touchmove', 200, 300, document.body));
		expect(uiStore.sidebarOpen).toBe(false);
	});
});
