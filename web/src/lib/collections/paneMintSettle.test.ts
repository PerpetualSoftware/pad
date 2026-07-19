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

	it('settles to null when a held-Back burst ends on the pane closing', () => {
		const onSettle = vi.fn();
		const settle = createPaneMintSettle({ onSettle });

		settle.onNavigate('popstate', 'TASK-2');
		vi.advanceTimersByTime(50);
		settle.onNavigate('popstate', null);
		vi.advanceTimersByTime(PANE_MINT_SETTLE_MS);

		expect(onSettle).toHaveBeenCalledTimes(1);
		expect(onSettle).toHaveBeenCalledWith(null);
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
