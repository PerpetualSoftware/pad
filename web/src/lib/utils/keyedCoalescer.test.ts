import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { createKeyedCoalescer, CoalescerCancelled } from './keyedCoalescer';

describe('createKeyedCoalescer (TASK-2224)', () => {
	beforeEach(() => vi.useFakeTimers());
	afterEach(() => vi.useRealTimers());

	it('a burst of calls for one key runs once, after the last call, and every caller gets its result', async () => {
		const fn = vi.fn(async (k: string) => `ran ${k}`);
		const c = createKeyedCoalescer(fn, { waitMs: 300, maxWaitMs: 1000 });
		const ps = [c.run('a'), c.run('a'), c.run('a')];
		await vi.advanceTimersByTimeAsync(299);
		expect(fn).not.toHaveBeenCalled();
		await vi.advanceTimersByTimeAsync(1);
		expect(fn).toHaveBeenCalledTimes(1);
		expect(await Promise.all(ps)).toEqual(['ran a', 'ran a', 'ran a']);
	});

	it('a continuous stream cannot defer the run past maxWaitMs', async () => {
		const fn = vi.fn(async () => 1);
		const c = createKeyedCoalescer(fn, { waitMs: 300, maxWaitMs: 1000 });
		for (let t = 0; t < 1000; t += 100) {
			void c.run('a');
			await vi.advanceTimersByTimeAsync(100);
		}
		expect(fn).toHaveBeenCalledTimes(1);
	});

	it('keys are independent, and a call after a run starts arms a new one', async () => {
		const fn = vi.fn(async (k: string) => k);
		const c = createKeyedCoalescer(fn, { waitMs: 300, maxWaitMs: 1000 });
		void c.run('a');
		void c.run('b');
		await vi.advanceTimersByTimeAsync(300);
		expect(fn.mock.calls.map((a) => a[0]).sort()).toEqual(['a', 'b']);
		void c.run('a');
		await vi.advanceTimersByTimeAsync(300);
		expect(fn).toHaveBeenCalledTimes(3);
	});

	it('cancel drops a pending run and settles its callers, rejected (codex r1)', async () => {
		const fn = vi.fn(async () => 1);
		const c = createKeyedCoalescer(fn, { waitMs: 300, maxWaitMs: 1000 });
		const a = c.run('a').catch((e) => e);
		const b = c.run('b').catch((e) => e);
		c.cancel('a');
		expect(await a).toBeInstanceOf(CoalescerCancelled);
		c.cancelAll();
		expect(await b).toBeInstanceOf(CoalescerCancelled);
		await vi.advanceTimersByTimeAsync(2000);
		expect(fn).not.toHaveBeenCalled();
	});

	it('a solitary call is capped by maxWaitMs too (codex r1)', async () => {
		const fn = vi.fn(async () => 1);
		const c = createKeyedCoalescer(fn, { waitMs: 5000, maxWaitMs: 1000 });
		void c.run('a');
		await vi.advanceTimersByTimeAsync(1000);
		expect(fn).toHaveBeenCalledTimes(1);
	});

	it('a failing run rejects every joined caller', async () => {
		const c = createKeyedCoalescer(async () => {
			throw new Error('boom');
		}, { waitMs: 10, maxWaitMs: 100 });
		const ps = [c.run('a'), c.run('a')].map((p) => p.catch((e: Error) => e.message));
		await vi.advanceTimersByTimeAsync(10);
		expect(await Promise.all(ps)).toEqual(['boom', 'boom']);
	});

	it('cancelling a run already in flight rejects its callers at once, and its result is discarded (codex r3)', async () => {
		let finish!: (v: number) => void;
		const fn = vi.fn(() => new Promise<number>((r) => (finish = r)));
		const c = createKeyedCoalescer(fn, { waitMs: 10, maxWaitMs: 100 });
		const p = c.run('a').then((v) => ({ v }), (e) => ({ e }));
		await vi.advanceTimersByTimeAsync(10);
		expect(fn).toHaveBeenCalledTimes(1); // in flight
		c.cancelAll();
		const got = await p;
		expect((got as { e?: unknown }).e).toBeInstanceOf(CoalescerCancelled);
		finish(42); // the late result reaches nobody
	});
});
