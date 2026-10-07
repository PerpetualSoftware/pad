// A keyed trailing debounce with a maximum wait, whose callers SHARE one
// promise (TASK-2224). Every call for a key while a run is pending joins it;
// the run starts `waitMs` after the LAST call, or `maxWaitMs` after the FIRST,
// whichever comes first, so a continuous stream of calls cannot defer it
// forever. A call after the run has started arms a new one.
//
// Used where an SSE burst asks for the same reload once per event: an agent's
// 20 updates in a second used to cost 20 reloads.
export interface KeyedCoalescer<K, T> {
	run(key: K): Promise<T>;
	/** Drop a pending run without starting it; its joined callers are rejected with CoalescerCancelled. */
	cancel(key: K): void;
	cancelAll(): void;
}

/** The rejection a joined caller gets when its pending run is cancelled. */
export class CoalescerCancelled extends Error {
	constructor() {
		super('coalesced run cancelled');
		this.name = 'CoalescerCancelled';
	}
}

export function createKeyedCoalescer<K, T>(
	fn: (key: K) => Promise<T>,
	opts: { waitMs: number; maxWaitMs: number },
): KeyedCoalescer<K, T> {
	interface Pending {
		promise: Promise<T>;
		resolve: (v: T) => void;
		reject: (e: unknown) => void;
		timer: ReturnType<typeof setTimeout>;
		firstAt: number;
	}
	const pending = new Map<K, Pending>();

	function fire(key: K) {
		const p = pending.get(key);
		if (!p) return;
		pending.delete(key);
		fn(key).then(p.resolve, p.reject);
	}

	return {
		run(key) {
			const now = Date.now();
			const existing = pending.get(key);
			if (existing) {
				clearTimeout(existing.timer);
				const wait = Math.max(0, Math.min(opts.waitMs, existing.firstAt + opts.maxWaitMs - now));
				existing.timer = setTimeout(() => fire(key), wait);
				return existing.promise;
			}
			let resolve!: (v: T) => void;
			let reject!: (e: unknown) => void;
			const promise = new Promise<T>((res, rej) => {
				resolve = res;
				reject = rej;
			});
			// A solitary call is capped by maxWaitMs too (codex r1).
			const timer = setTimeout(() => fire(key), Math.min(opts.waitMs, opts.maxWaitMs));
			pending.set(key, { promise, resolve, reject, timer, firstAt: now });
			return promise;
		},
		// Every joined caller is SETTLED, rejected, so nothing waits forever on
		// a run that will never start (codex r1).
		cancel(key) {
			const p = pending.get(key);
			if (p) {
				clearTimeout(p.timer);
				pending.delete(key);
				p.reject(new CoalescerCancelled());
			}
		},
		cancelAll() {
			for (const p of pending.values()) {
				clearTimeout(p.timer);
				p.reject(new CoalescerCancelled());
			}
			pending.clear();
		},
	};
}
