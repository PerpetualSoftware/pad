import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { SAVED_MS, SaveTracker } from './saveTracker.svelte';

/**
 * BUG-3044 — the pane's save indicator describes the writes outstanding. The
 * first three legs are the three filed sequences; each was reachable with the
 * single last-writer `saveStatus` it replaces.
 */

beforeEach(() => {
	vi.useFakeTimers();
});
afterEach(() => {
	vi.useRealTimers();
});

describe('SaveTracker (BUG-3044)', () => {
	it('an older success does not show Saved while a newer write is in flight', () => {
		const s = new SaveTracker();
		const older = s.begin();
		const newer = s.begin();
		s.succeed(older);
		s.settle(older);
		expect(s.status).toBe('saving');
		s.succeed(newer);
		s.settle(newer);
		expect(s.status).toBe('saved');
	});

	it('an older success settling AFTER the newest write failed does not show Saved', () => {
		const s = new SaveTracker();
		const older = s.begin();
		const newer = s.begin();
		s.settle(newer); // failed
		expect(s.status).toBe('saving');
		s.succeed(older);
		s.settle(older);
		expect(s.status).toBe('idle');
	});

	it("one field's failure does not hide another field's write in flight", () => {
		const s = new SaveTracker();
		const a = s.begin();
		const b = s.begin();
		s.settle(a); // a failed
		expect(s.status).toBe('saving');
		s.succeed(b);
		s.settle(b);
		expect(s.status).toBe('saved');
	});

	it("a previous save's Saved timer does not fire during a new request", () => {
		const s = new SaveTracker();
		const first = s.begin();
		s.succeed(first);
		s.settle(first);
		expect(s.status).toBe('saved');
		vi.advanceTimersByTime(SAVED_MS - 100);
		const second = s.begin();
		vi.advanceTimersByTime(500); // the first save's timer would have fired here
		expect(s.status).toBe('saving');
		s.succeed(second);
		s.settle(second);
		expect(s.status).toBe('saved');
		vi.advanceTimersByTime(SAVED_MS);
		expect(s.status).toBe('idle');
	});

	it('a superseded write that settles in a finally does not pin saving', () => {
		// The trap a plain counter walks into: a superseded write returns early
		// without touching the status. Settled from a finally, it still counts down.
		const s = new SaveTracker();
		const superseded = s.begin();
		const winner = s.begin();
		s.succeed(winner);
		s.settle(winner);
		expect(s.status).toBe('saving');
		s.settle(superseded);
		expect(s.status).toBe('saved');
	});

	it('a settle from before a reset (item switch) is ignored', () => {
		const s = new SaveTracker();
		const old = s.begin();
		s.reset();
		expect(s.status).toBe('idle');
		const current = s.begin();
		s.succeed(old);
		s.settle(old);
		expect(s.status, 'the previous item cannot end this one').toBe('saving');
		s.settle(current);
		expect(s.status).toBe('idle');
	});

	it('settling a token twice counts once', () => {
		const s = new SaveTracker();
		const a = s.begin();
		const b = s.begin();
		s.settle(a);
		s.settle(a);
		expect(s.status).toBe('saving');
		s.settle(b);
		expect(s.status).toBe('idle');
	});

	it('reset clears a pending Saved timer', () => {
		const s = new SaveTracker();
		const t = s.begin();
		s.succeed(t);
		s.settle(t);
		s.reset();
		const next = s.begin();
		vi.advanceTimersByTime(SAVED_MS + 1);
		expect(s.status).toBe('saving');
		s.settle(next);
	});
});
