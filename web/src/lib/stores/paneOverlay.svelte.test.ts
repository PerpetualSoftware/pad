// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`), which
// compiles the `.svelte.ts` rune module. `paneOverlay` is a ref-counted signal
// (TASK-2131): PaneHost calls enter()/leave() per active mobile overlay and the
// workspace layout reads `mobileOverlayActive` to inert the app-shell chrome.
// The module holds process-wide `$state`, so each test re-imports a fresh copy.
import { describe, it, expect, beforeEach, vi } from 'vitest';

beforeEach(() => {
	// Fresh module (and its process-wide $state count) per test.
	vi.resetModules();
});

describe('paneOverlay', () => {
	it('starts inactive', async () => {
		const { paneOverlay } = await import('./paneOverlay.svelte');
		expect(paneOverlay.mobileOverlayActive).toBe(false);
	});

	it('is active after a single enter() and inactive after the matching leave()', async () => {
		const { paneOverlay } = await import('./paneOverlay.svelte');
		paneOverlay.enter();
		expect(paneOverlay.mobileOverlayActive).toBe(true);
		paneOverlay.leave();
		expect(paneOverlay.mobileOverlayActive).toBe(false);
	});

	it('stays active while overlapping overlays are open (ref-counted)', async () => {
		// Models a route change where the incoming PaneHost enters before the
		// outgoing one's effect cleanup leaves — the signal must not flicker off.
		const { paneOverlay } = await import('./paneOverlay.svelte');
		paneOverlay.enter(); // outgoing host
		paneOverlay.enter(); // incoming host mounts before...
		expect(paneOverlay.mobileOverlayActive).toBe(true);
		paneOverlay.leave(); // ...outgoing host's cleanup runs
		expect(paneOverlay.mobileOverlayActive).toBe(true);
		paneOverlay.leave();
		expect(paneOverlay.mobileOverlayActive).toBe(false);
	});

	it('never goes negative on an unmatched leave()', async () => {
		// A defensive floor: a stray leave() (double-cleanup) can't drive the
		// count below zero and wedge a later enter() into staying inactive.
		const { paneOverlay } = await import('./paneOverlay.svelte');
		paneOverlay.leave();
		expect(paneOverlay.mobileOverlayActive).toBe(false);
		paneOverlay.enter();
		expect(paneOverlay.mobileOverlayActive).toBe(true);
		paneOverlay.leave();
		expect(paneOverlay.mobileOverlayActive).toBe(false);
	});
});
