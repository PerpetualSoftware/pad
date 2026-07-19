import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { createPaneMintSettle, PANE_MINT_SETTLE_MS } from './paneMintSettle';

describe('createPaneMintSettle', () => {
	beforeEach(() => {
		vi.useFakeTimers();
	});
	afterEach(() => {
		vi.useRealTimers();
	});

	it('coalesces a held-Back popstate burst into a single settle on the final ref', () => {
		const onSettle = vi.fn();
		const settle = createPaneMintSettle({ onSettle });

		settle.onNavigate('popstate', 'TASK-3');
		vi.advanceTimersByTime(50);
		settle.onNavigate('popstate', 'TASK-2'); // supersedes TASK-3 before it fires
		vi.advanceTimersByTime(50);
		settle.onNavigate('popstate', 'TASK-1');

		// Nothing fires until a full settle window elapses after the LAST popstate.
		expect(onSettle).not.toHaveBeenCalled();
		vi.advanceTimersByTime(PANE_MINT_SETTLE_MS);

		expect(onSettle).toHaveBeenCalledTimes(1);
		expect(onSettle).toHaveBeenCalledWith('TASK-1');
	});

	it('applies a non-popstate nav (explicit open/drill/close) immediately, no settle delay', () => {
		const onSettle = vi.fn();
		const settle = createPaneMintSettle({ onSettle });

		settle.onNavigate('goto', 'TASK-9');

		expect(onSettle).toHaveBeenCalledTimes(1);
		expect(onSettle).toHaveBeenCalledWith('TASK-9');
	});

	it('a deliberate nav mid-burst cancels the pending popstate settle and applies right away', () => {
		const onSettle = vi.fn();
		const settle = createPaneMintSettle({ onSettle });

		settle.onNavigate('popstate', 'TASK-2'); // held Back in flight
		vi.advanceTimersByTime(50);
		settle.onNavigate('goto', 'TASK-7'); // e.g. the reset-latch write mid-traversal

		expect(onSettle).toHaveBeenCalledTimes(1);
		expect(onSettle).toHaveBeenCalledWith('TASK-7');

		// The superseded popstate timer must not fire a second, stale settle.
		vi.advanceTimersByTime(PANE_MINT_SETTLE_MS);
		expect(onSettle).toHaveBeenCalledTimes(1);
	});

	it('a popstate that closes the pane (ref -> null) applies IMMEDIATELY, never settled', () => {
		// The pane's mount boundary (`{#if openItemRef}` in +page.svelte)
		// reacts to the raw URL instantly and tears `<ItemDetail>` down the
		// moment `?item=` disappears — a delayed null would leave a stale
		// non-null `paneMintRef` around to remount against on a quick
		// close-then-reopen (the P2 this guards against; see the next spec).
		const onSettle = vi.fn();
		const settle = createPaneMintSettle({ onSettle });

		settle.onNavigate('popstate', 'TASK-2'); // held Back, still mid-drill
		vi.advanceTimersByTime(50);
		settle.onNavigate('popstate', null); // Back crosses the pane-closed boundary

		// Fires right away — no need to advance the fake clock at all.
		expect(onSettle).toHaveBeenCalledTimes(1);
		expect(onSettle).toHaveBeenCalledWith(null);

		// The superseded TASK-2 timer must not fire a stale second call.
		vi.advanceTimersByTime(PANE_MINT_SETTLE_MS);
		expect(onSettle).toHaveBeenCalledTimes(1);
	});

	it('a quick close-then-reopen (ref -> null -> different ref) settles cleanly with no stale intermediate mint', () => {
		const onSettle = vi.fn();
		const settle = createPaneMintSettle({ onSettle });

		settle.onNavigate('popstate', 'TASK-2'); // pane open, drilling
		settle.onNavigate('popstate', null); // Back closes — applies immediately
		expect(onSettle).toHaveBeenNthCalledWith(1, null);

		// Forward reopens on a DIFFERENT item, itself a held burst.
		settle.onNavigate('popstate', 'TASK-9');
		vi.advanceTimersByTime(50);
		settle.onNavigate('popstate', 'TASK-10');
		// Still just the close call — the reopen burst hasn't settled yet, and
		// critically never applied the stale 'TASK-2' or the intermediate
		// 'TASK-9'.
		expect(onSettle).toHaveBeenCalledTimes(1);

		vi.advanceTimersByTime(PANE_MINT_SETTLE_MS);
		expect(onSettle).toHaveBeenCalledTimes(2);
		expect(onSettle).toHaveBeenNthCalledWith(2, 'TASK-10');
	});

	it('cancel() drops a pending settle without applying it', () => {
		const onSettle = vi.fn();
		const settle = createPaneMintSettle({ onSettle });

		settle.onNavigate('popstate', 'TASK-2');
		settle.cancel();
		vi.advanceTimersByTime(5000);

		expect(onSettle).not.toHaveBeenCalled();
	});

	it('uses a custom settleMs when supplied', () => {
		const onSettle = vi.fn();
		const settle = createPaneMintSettle({ onSettle, settleMs: 500 });

		settle.onNavigate('popstate', 'TASK-1');
		vi.advanceTimersByTime(499);
		expect(onSettle).not.toHaveBeenCalled();
		vi.advanceTimersByTime(1);
		expect(onSettle).toHaveBeenCalledTimes(1);
	});

	it('a single isolated popstate (not a burst) still settles after the window', () => {
		const onSettle = vi.fn();
		const settle = createPaneMintSettle({ onSettle });

		settle.onNavigate('popstate', 'TASK-5');
		vi.advanceTimersByTime(PANE_MINT_SETTLE_MS);

		expect(onSettle).toHaveBeenCalledTimes(1);
		expect(onSettle).toHaveBeenCalledWith('TASK-5');
	});
});
