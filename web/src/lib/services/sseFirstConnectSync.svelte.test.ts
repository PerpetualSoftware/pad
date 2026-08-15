// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`) because
// sse.svelte.ts uses runes.
//
// WHAT THIS PINS (BUG-2540). A page reads its items and only THEN subscribes to
// SSE. A mutation landing in that window reaches nobody: no subscription exists
// yet, so the frame is never received, and nothing reconciles the gap
// afterwards — the row stays stale until some unrelated event happens to
// trigger a sync.
//
// `pendingSyncOnConnect` was the right mechanism but was armed only on LEADER
// PROMOTION (and the lock-failure fallback), so the FIRST connect — the one
// every page load performs — was uncovered. These tests assert it now fires on
// every connect, and, importantly, that it still fires AFTER the stream is
// open rather than before: firing early would just move the same gap, which is
// the property TASK-1359 round 4 established and this change must not lose.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

/** The fake EventSource instances a test has caused to be constructed. */
let sources: FakeEventSource[] = [];

class FakeEventSource {
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
	/** Drive the platform's open signal. */
	fireOpen() {
		this.readyState = 1;
		this.onopen?.();
	}
	/** Drive the server's own `connected` frame (the other arm). */
	fireConnected() {
		for (const cb of this.listeners.get('connected') ?? []) cb({});
	}
}

/**
 * Minimal `navigator.locks`. `request` grants immediately and never resolves
 * the held promise, matching the real lock's lifetime (the service holds it
 * until disconnect).
 */
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
	// `delete navigator.locks` is what makes leaderElectionSupported() false —
	// `'locks' in navigator` is the probe, so an undefined VALUE is not enough.
	// @ts-expect-error — deleting an optional platform property under test
	delete globalThis.navigator.locks;
}

async function loadService() {
	vi.resetModules();
	return (await import('./sse.svelte')).sseService;
}

beforeEach(() => {
	sources = [];
	vi.stubGlobal('EventSource', FakeEventSource);
	installLocks();
});

afterEach(() => {
	vi.unstubAllGlobals();
	removeLocks();
});

describe('SSE first-connect sync coverage (BUG-2540)', () => {
	it('fires sync_required on the FIRST connect, not only on leader promotion', async () => {
		const sse = await loadService();
		const onSync = vi.fn();
		sse.onSyncRequired(onSync);

		sse.connect('ws-a');
		// The lock grant runs the callback in a microtask.
		await Promise.resolve();
		await Promise.resolve();

		expect(sources).toHaveLength(1);
		// Nothing yet — the whole point is that it waits for the stream.
		expect(onSync).not.toHaveBeenCalled();

		sources[0].fireOpen();
		expect(onSync).toHaveBeenCalledTimes(1);

		sse.disconnect();
	});

	it('fires it AFTER the stream opens, never before', async () => {
		// The ordering property from TASK-1359 round 4. Dispatching early would
		// relocate the gap rather than close it: the resulting /items-changes
		// snapshot would be taken before the subscription exists, so a mutation
		// between snapshot and subscription would still reach nobody.
		const sse = await loadService();
		const order: string[] = [];
		sse.onSyncRequired(() => order.push('sync'));

		sse.connect('ws-b');
		await Promise.resolve();
		await Promise.resolve();
		order.push('constructed');

		sources[0].fireOpen();
		order.push('opened');

		expect(order).toEqual(['constructed', 'sync', 'opened']);
		sse.disconnect();
	});

	it('is claimed by whichever of onopen / `connected` arrives first, and only once', async () => {
		const sse = await loadService();
		const onSync = vi.fn();
		sse.onSyncRequired(onSync);

		sse.connect('ws-c');
		await Promise.resolve();
		await Promise.resolve();

		// Some platforms deliver the server's `connected` frame before onopen.
		sources[0].fireConnected();
		expect(onSync).toHaveBeenCalledTimes(1);

		// The other arm must not fire a second, redundant sync.
		sources[0].fireOpen();
		expect(onSync).toHaveBeenCalledTimes(1);

		sse.disconnect();
	});

	it('arms on the no-leader-election fallback path too', async () => {
		// Browsers without navigator.locks skip leader election entirely and
		// every tab opens its own EventSource. That path reaches openEventSource
		// by a different route, so it needs its own arming — and its own test,
		// since the lock-path tests above can't exercise it.
		removeLocks();
		const sse = await loadService();
		const onSync = vi.fn();
		sse.onSyncRequired(onSync);

		sse.connect('ws-d');
		await Promise.resolve();

		expect(sources).toHaveLength(1);
		sources[0].fireOpen();
		expect(onSync).toHaveBeenCalledTimes(1);

		sse.disconnect();
	});

	it('re-arms for the next connect, so a workspace switch is covered too', async () => {
		const sse = await loadService();
		const onSync = vi.fn();
		sse.onSyncRequired(onSync);

		sse.connect('ws-e');
		await Promise.resolve();
		await Promise.resolve();
		sources[0].fireOpen();
		expect(onSync).toHaveBeenCalledTimes(1);

		// A switch tears down and reconnects — the new workspace's initial read
		// has exactly the same gap as the first one did.
		sse.connect('ws-f');
		await Promise.resolve();
		await Promise.resolve();
		expect(sources).toHaveLength(2);
		sources[1].fireOpen();
		expect(onSync).toHaveBeenCalledTimes(2);

		sse.disconnect();
	});
});
