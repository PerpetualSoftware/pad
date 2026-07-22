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

// NOTE (BUG-2284): the mutators `untrack` their `overlayCount` read so a
// caller writing them from inside an `$effect` (PaneHost is the only writer)
// can't take a reactive dependency on the signal it writes — which would loop
// (`effect_update_depth_exceeded`) and abort the flush. That runaway only
// manifests under the real browser scheduler, not jsdom/vitest, so the loop
// regression is guarded by the E2E `pane-controller.spec.ts` mobile-overlay
// tests. The ref-count semantics below still lock the mutators' behavior so the
// untrack change can't silently break counting.

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
