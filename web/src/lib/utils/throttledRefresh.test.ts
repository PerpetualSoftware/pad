import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createThrottledRefresh } from './throttledRefresh';

// BUG-3160: the cap the lead ruled — at most one head re-read per 10s per view,
// trailing, none while hidden, one catch-up on becoming visible.

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

function setup(hidden = { value: false }) {
	const run = vi.fn();
	const t = createThrottledRefresh(run, { intervalMs: 10_000, isHidden: () => hidden.value, now: () => Date.now() });
	return { run, t, hidden };
}

describe('createThrottledRefresh', () => {
	it('a burst of events costs at most one run per 10s — the pinned rate (≤6/min)', () => {
		const { run, t } = setup();
		// Two busy seats: an event every 250ms for a full minute (240 events).
		for (let ms = 0; ms < 60_000; ms += 250) {
			t.trigger();
			vi.advanceTimersByTime(250);
		}
		vi.advanceTimersByTime(10_000); // let the trailing run land
		expect(run.mock.calls.length).toBeLessThanOrEqual(7); // 6 in the minute + the trailing one after it
		expect(run.mock.calls.length).toBeGreaterThanOrEqual(6);
	});

	it('the last event of a burst is always followed by a run (trailing edge)', () => {
		const { run, t } = setup();
		t.trigger();
		vi.advanceTimersByTime(500);
		expect(run).toHaveBeenCalledTimes(1);
		vi.advanceTimersByTime(1_000);
		t.trigger(); // inside the interval
		vi.advanceTimersByTime(8_000);
		expect(run, 'must not run before the interval since the last run').toHaveBeenCalledTimes(1);
		vi.advanceTimersByTime(1_000);
		expect(run, 'the trailing run after the last event').toHaveBeenCalledTimes(2);
	});

	it('a lone event after quiet runs after the short coalescing delay, not 10s', () => {
		const { run, t } = setup();
		t.trigger();
		vi.advanceTimersByTime(499);
		expect(run).not.toHaveBeenCalled();
		vi.advanceTimersByTime(1);
		expect(run).toHaveBeenCalledTimes(1);
	});

	it('nothing runs while hidden; one catch-up on visible, only if an event was missed', () => {
		const { run, t, hidden } = setup({ value: true });
		t.trigger();
		t.trigger();
		vi.advanceTimersByTime(30_000);
		expect(run).not.toHaveBeenCalled();
		hidden.value = false;
		t.onVisible();
		vi.advanceTimersByTime(500);
		expect(run).toHaveBeenCalledTimes(1);
		// Becoming visible again with nothing missed runs nothing.
		t.onVisible();
		vi.advanceTimersByTime(30_000);
		expect(run).toHaveBeenCalledTimes(1);
	});

	it('a run that comes due while hidden is deferred to the catch-up, not dropped', () => {
		const { run, t, hidden } = setup();
		t.trigger();
		hidden.value = true;
		vi.advanceTimersByTime(500);
		expect(run).not.toHaveBeenCalled();
		hidden.value = false;
		t.onVisible();
		vi.advanceTimersByTime(500);
		expect(run).toHaveBeenCalledTimes(1);
	});

	it('dispose clears the pending run and ignores later triggers', () => {
		const { run, t } = setup();
		t.trigger();
		t.dispose();
		vi.advanceTimersByTime(30_000);
		t.trigger();
		t.onVisible();
		vi.advanceTimersByTime(30_000);
		expect(run).not.toHaveBeenCalled();
	});
});
