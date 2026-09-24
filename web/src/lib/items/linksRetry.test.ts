import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createLinksRetry, LINKS_RETRY_DELAYS_MS } from './linksRetry';

const A = { itemId: 'item-a', ws: 'ws' };
const B = { itemId: 'item-b', ws: 'ws' };

/** An attempt whose answers are scripted, recording who it was asked about. */
function scripted(...answers: boolean[]) {
	const calls: string[] = [];
	const attempt = vi.fn(async (t: { itemId: string }) => {
		calls.push(t.itemId);
		return answers.length ? answers.shift()! : true;
	});
	return { attempt, calls };
}

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

describe('createLinksRetry (BUG-2992)', () => {
	it('schedules a retry after a failure; nothing fires before the first delay', async () => {
		const { attempt, calls } = scripted(true);
		const r = createLinksRetry(attempt);
		r.failed(A);
		expect(r.pending).toBe(true);
		await vi.advanceTimersByTimeAsync(LINKS_RETRY_DELAYS_MS[0] - 1);
		expect(calls).toEqual([]);
		await vi.advanceTimersByTimeAsync(1);
		expect(calls).toEqual(['item-a']);
		expect(r.pending).toBe(false);
	});

	it('backs off through every delay while the attempt keeps failing, then stops', async () => {
		const { attempt, calls } = scripted(false, false, false, false);
		const r = createLinksRetry(attempt);
		r.failed(A);
		for (const d of LINKS_RETRY_DELAYS_MS) await vi.advanceTimersByTimeAsync(d);
		expect(calls).toHaveLength(LINKS_RETRY_DELAYS_MS.length);
		// Spent: an hour more of wall clock asks nothing further…
		await vi.advanceTimersByTimeAsync(3_600_000);
		expect(calls).toHaveLength(LINKS_RETRY_DELAYS_MS.length);
		// …but the debt is still recorded, which is what `kick()` acts on.
		expect(r.pending).toBe(true);
	});

	it('kick() restarts a spent schedule immediately', async () => {
		const { attempt, calls } = scripted(false, false, false, true);
		const r = createLinksRetry(attempt);
		r.failed(A);
		for (const d of LINKS_RETRY_DELAYS_MS) await vi.advanceTimersByTimeAsync(d);
		expect(calls).toHaveLength(3);
		r.kick();
		await vi.advanceTimersByTimeAsync(0);
		expect(calls).toHaveLength(4);
		expect(r.pending).toBe(false);
	});

	it('a new failure after a spent schedule starts it again', async () => {
		const { attempt, calls } = scripted(false, false, false, true);
		const r = createLinksRetry(attempt);
		r.failed(A);
		for (const d of LINKS_RETRY_DELAYS_MS) await vi.advanceTimersByTimeAsync(d);
		r.failed(A);
		await vi.advanceTimersByTimeAsync(LINKS_RETRY_DELAYS_MS[0]);
		expect(calls).toHaveLength(4);
	});

	it('a success for the item drops the owed retry', async () => {
		const { attempt, calls } = scripted();
		const r = createLinksRetry(attempt);
		r.failed(A);
		r.succeeded('item-a');
		await vi.advanceTimersByTimeAsync(3_600_000);
		expect(calls).toEqual([]);
		expect(r.pending).toBe(false);
	});

	it('a success for a DIFFERENT item does not drop it', async () => {
		const { attempt, calls } = scripted(true);
		const r = createLinksRetry(attempt);
		r.failed(A);
		r.succeeded('item-b');
		await vi.advanceTimersByTimeAsync(LINKS_RETRY_DELAYS_MS[0]);
		expect(calls).toEqual(['item-a']);
	});

	it('a failure for another item replaces the target and restarts the backoff', async () => {
		const { attempt, calls } = scripted(false, true);
		const r = createLinksRetry(attempt);
		r.failed(A);
		await vi.advanceTimersByTimeAsync(LINKS_RETRY_DELAYS_MS[0]); // A fails once
		r.failed(B);
		await vi.advanceTimersByTimeAsync(LINKS_RETRY_DELAYS_MS[0]);
		expect(calls).toEqual(['item-a', 'item-b']);
	});

	it('a failure landing WHILE an attempt is in flight is not lost', async () => {
		// The attempt that is running answers "fresh" for the target it was
		// given, but a newer failure replaced that target mid-flight, so the
		// debt belongs to the newer one and must stay scheduled.
		let release!: (v: boolean) => void;
		const calls: string[] = [];
		const attempt = vi.fn((t: { itemId: string }) => {
			calls.push(t.itemId);
			return calls.length === 1 ? new Promise<boolean>((res) => (release = res)) : Promise.resolve(true);
		});
		const r = createLinksRetry(attempt);
		r.failed(A);
		await vi.advanceTimersByTimeAsync(LINKS_RETRY_DELAYS_MS[0]);
		r.failed({ ...A });
		release(true);
		await vi.advanceTimersByTimeAsync(0);
		expect(r.pending).toBe(true);
		// Same item, so the backoff continues where it was: the second delay.
		await vi.advanceTimersByTimeAsync(LINKS_RETRY_DELAYS_MS[1] - 1);
		expect(calls).toHaveLength(1);
		await vi.advanceTimersByTimeAsync(1);
		expect(calls).toHaveLength(2);
		expect(r.pending).toBe(false);
	});

	it('a throwing attempt counts as a failure, not as fresh', async () => {
		const attempt = vi.fn().mockRejectedValueOnce(new Error('boom')).mockResolvedValue(true);
		const r = createLinksRetry(attempt);
		r.failed(A);
		await vi.advanceTimersByTimeAsync(LINKS_RETRY_DELAYS_MS[0]);
		expect(r.pending).toBe(true);
		await vi.advanceTimersByTimeAsync(LINKS_RETRY_DELAYS_MS[1]);
		expect(attempt).toHaveBeenCalledTimes(2);
		expect(r.pending).toBe(false);
	});

	it('cancel() stops everything', async () => {
		const { attempt, calls } = scripted();
		const r = createLinksRetry(attempt);
		r.failed(A);
		r.cancel();
		r.kick();
		await vi.advanceTimersByTimeAsync(3_600_000);
		expect(calls).toEqual([]);
	});

	it('kick() with nothing owed asks nothing', async () => {
		const { attempt } = scripted();
		createLinksRetry(attempt).kick();
		await vi.advanceTimersByTimeAsync(0);
		expect(attempt).not.toHaveBeenCalled();
	});
});

