// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`) because
// sse.svelte.ts uses runes.
//
// WHAT THIS PINS (BUG-2733). Measured in Chromium on the bug's trail: a
// connection the server REFUSES (429 / 503) is never retried by EventSource, so
// the tab went dark for good while its indicator said "reconnecting"; a stream
// that ENDS is retried on a flat 3s, so every tab of a restarted server came
// back on one tick. The service now owns the reconnect. The fake below has no
// auto-reconnect of its own, which is the refused case exactly and makes every
// new source in these tests one the SERVICE opened.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { parseRetryAfterMs, reconnectDelayMs, RECONNECT_BASE_MS, RECONNECT_CAP_MS } from './sseReconnect';

let sources: FakeEventSource[] = [];

class FakeEventSource {
	static readonly CONNECTING = 0;
	static readonly OPEN = 1;
	static readonly CLOSED = 2;
	url: string;
	readyState = 0;
	onopen: (() => void) | null = null;
	onerror: (() => void) | null = null;
	closed = false;
	private listeners = new Map<string, Set<(e: unknown) => void>>();

	constructor(url: string) {
		this.url = url;
		sources.push(this);
	}
	addEventListener(type: string, cb: (e: unknown) => void) {
		let set = this.listeners.get(type);
		if (!set) {
			set = new Set();
			this.listeners.set(type, set);
		}
		set.add(cb);
	}
	close() {
		this.closed = true;
		this.readyState = FakeEventSource.CLOSED;
	}
	fireOpen() {
		this.readyState = 1;
		this.onopen?.();
	}
	/** A refused connection: the browser sets CLOSED, then fires error once. */
	fireRefused() {
		this.readyState = FakeEventSource.CLOSED;
		this.onerror?.();
	}
	/** An established stream that ends: the browser goes CONNECTING and fires error. */
	fireDropped() {
		this.readyState = FakeEventSource.CONNECTING;
		this.onerror?.();
	}
	fire(type: string, data?: unknown) {
		for (const cb of this.listeners.get(type) ?? []) {
			cb(data === undefined ? {} : { data: JSON.stringify(data) });
		}
	}
}

function installLocks() {
	Object.defineProperty(globalThis.navigator, 'locks', {
		value: {
			request: (_name: string, _opts: unknown, cb: () => Promise<void>) => {
				void cb();
				return new Promise<void>(() => {});
			},
			query: async () => ({ held: [], pending: [] })
		},
		configurable: true,
		writable: true
	});
}

function removeLocks() {
	// @ts-expect-error — deleting an optional platform property under test
	delete globalThis.navigator.locks;
}

async function loadService() {
	vi.resetModules();
	return (await import('./sse.svelte')).sseService;
}

async function flush() {
	for (let i = 0; i < 5; i++) await Promise.resolve();
}

async function connected(ws = 'ws-a') {
	const sse = await loadService();
	sse.connect(ws);
	await flush();
	expect(sources).toHaveLength(1);
	sources[0].fireOpen();
	return sse;
}

function refusalFetch(status: number, retryAfter: string | null) {
	return vi.fn(async () => new Response('{}', { status, headers: retryAfter ? { 'Retry-After': retryAfter } : {} }));
}

beforeEach(() => {
	sources = [];
	vi.useFakeTimers();
	vi.stubGlobal('EventSource', FakeEventSource);
	installLocks();
});

afterEach(() => {
	vi.useRealTimers();
	vi.unstubAllGlobals();
	vi.restoreAllMocks();
	removeLocks();
});

describe('reconnect policy (BUG-2733)', () => {
	it('follows the CLI ladder: 5s linear, capped at 5min, jitter upward only', () => {
		const none = () => 0;
		const most = () => 0.999999;
		expect(reconnectDelayMs(1, null, none)).toBe(RECONNECT_BASE_MS);
		expect(reconnectDelayMs(3, null, none)).toBe(3 * RECONNECT_BASE_MS);
		expect(reconnectDelayMs(1000, null, none)).toBe(RECONNECT_CAP_MS);
		// Never earlier than the step, never more than half again.
		expect(reconnectDelayMs(2, null, most)).toBeLessThanOrEqual(2 * RECONNECT_BASE_MS * 1.5);
		expect(reconnectDelayMs(2, null, most)).toBeGreaterThan(2 * RECONNECT_BASE_MS);
	});

	it('never comes back earlier than a Retry-After, even with jitter at its lowest', () => {
		expect(reconnectDelayMs(1, 30_000, () => 0)).toBe(30_000);
		// A Retry-After below the step does not lower the step.
		expect(reconnectDelayMs(3, 1_000, () => 0)).toBe(3 * RECONNECT_BASE_MS);
	});

	it('parses both Retry-After forms, and refuses nonsense', () => {
		const now = Date.UTC(2026, 8, 23, 6, 0, 0);
		expect(parseRetryAfterMs('7', now)).toBe(7000);
		expect(parseRetryAfterMs(new Date(now + 12_000).toUTCString(), now)).toBe(12_000);
		expect(parseRetryAfterMs(null, now)).toBeNull();
		expect(parseRetryAfterMs('soon', now)).toBeNull();
		expect(parseRetryAfterMs('0', now)).toBeNull();
		expect(parseRetryAfterMs(new Date(now - 5_000).toUTCString(), now)).toBeNull();
	});
});

describe('owned reconnect in the SSE service (BUG-2733)', () => {
	it('a REFUSED tab recovers on its own, no earlier than the Retry-After it was given', async () => {
		const fetchSpy = refusalFetch(429, '30');
		vi.stubGlobal('fetch', fetchSpy);
		const sse = await connected();

		sources[0].fireRefused();
		await flush();
		expect(fetchSpy).toHaveBeenCalledTimes(1);
		expect(sse.status).toBe('reconnecting');

		await vi.advanceTimersByTimeAsync(29_999);
		expect(sources).toHaveLength(1); // not before the server's floor

		await vi.advanceTimersByTimeAsync(30_000 * 0.5 + 1); // past the jitter ceiling
		expect(sources).toHaveLength(2); // before the fix: 1 forever
		sse.disconnect();
	});

	it('a 503 refusal is probed and recovers the same way', async () => {
		vi.stubGlobal('fetch', refusalFetch(503, '5'));
		const sse = await connected();
		sources[0].fireRefused();
		await flush();
		await vi.advanceTimersByTimeAsync(RECONNECT_BASE_MS * 1.5 + 1);
		expect(sources).toHaveLength(2);
		sse.disconnect();
	});

	it('a DROPPED stream is not probed, and backs off instead of a flat 3s', async () => {
		const fetchSpy = vi.fn();
		vi.stubGlobal('fetch', fetchSpy);
		const sse = await connected();

		sources[0].fireDropped();
		await flush();
		await vi.advanceTimersByTimeAsync(3_000);
		expect(sources).toHaveLength(1); // the browser's flat 3s is no longer the schedule
		await vi.advanceTimersByTimeAsync(RECONNECT_BASE_MS * 1.5);
		expect(sources).toHaveLength(2);

		// A second failure WITHOUT a successful connect in between climbs the ladder.
		sources[1].fireDropped();
		await flush();
		await vi.advanceTimersByTimeAsync(2 * RECONNECT_BASE_MS - 1);
		expect(sources).toHaveLength(2);
		await vi.advanceTimersByTimeAsync(RECONNECT_BASE_MS + 1);
		expect(sources).toHaveLength(3);

		expect(fetchSpy).not.toHaveBeenCalled();
		sse.disconnect();
	});

	it('a successful connect resets the ladder', async () => {
		vi.stubGlobal('fetch', vi.fn());
		const sse = await connected();
		sources[0].fireDropped();
		await flush();
		await vi.advanceTimersByTimeAsync(RECONNECT_BASE_MS * 1.5 + 1);
		expect(sources).toHaveLength(2);
		sources[1].fireOpen();

		sources[1].fireDropped();
		await flush();
		await vi.advanceTimersByTimeAsync(RECONNECT_BASE_MS * 1.5 + 1);
		expect(sources).toHaveLength(3); // back on the first step, not the second
		sse.disconnect();
	});

	it('two clients started together do not retry in lockstep', async () => {
		vi.stubGlobal('fetch', vi.fn());
		const delays: number[] = [];
		const realSetTimeout = globalThis.setTimeout;
		for (let client = 0; client < 2; client++) {
			sources = [];
			const spy = vi.spyOn(globalThis, 'setTimeout');
			const sse = await connected();
			sources[0].fireDropped();
			await flush();
			const reconnectCall = spy.mock.calls.find((c) => typeof c[1] === 'number' && c[1] >= RECONNECT_BASE_MS);
			expect(reconnectCall, 'no reconnect was scheduled').toBeDefined();
			delays.push(reconnectCall![1] as number);
			spy.mockRestore();
			sse.disconnect();
		}
		expect(globalThis.setTimeout).toBe(realSetTimeout);
		expect(delays[0]).not.toBe(delays[1]);
	});

	it('the replacement arms a connect-sync, because a new source sends no Last-Event-ID', async () => {
		vi.stubGlobal('fetch', vi.fn());
		const sse = await connected();
		const onSync = vi.fn();
		sse.onSyncRequired(onSync);
		// The first connect's sync (BUG-2540) is already claimed by fireOpen.
		onSync.mockClear();

		sources[0].fireDropped();
		await flush();
		await vi.advanceTimersByTimeAsync(RECONNECT_BASE_MS * 1.5 + 1);
		expect(onSync).not.toHaveBeenCalled(); // premise: nothing before the new source is subscribed
		sources[1].fireOpen();
		expect(onSync).toHaveBeenCalledTimes(1);
		sse.disconnect();
	});

	it('an unauthorized close stays closed', async () => {
		vi.stubGlobal('fetch', vi.fn());
		const sse = await connected();
		sources[0].fire('unauthorized');
		expect(sse.status).toBe('unauthorized');
		// Whatever the browser does after the server ends the stream, the
		// source is no longer the live one, so nothing reopens it.
		sources[0].fireDropped();
		await flush();
		await vi.advanceTimersByTimeAsync(RECONNECT_CAP_MS * 2);
		expect(sources).toHaveLength(1);
	});

	it('disconnect cancels a scheduled reconnect', async () => {
		vi.stubGlobal('fetch', vi.fn());
		const sse = await connected();
		sources[0].fireDropped();
		await flush();
		sse.disconnect();
		await vi.advanceTimersByTimeAsync(RECONNECT_CAP_MS * 2);
		expect(sources).toHaveLength(1);
	});
});
