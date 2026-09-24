// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`) because
// sse.svelte.ts uses runes.
//
// WHAT THIS PINS (BUG-2761). The server's mid-stream `sync_required` is sent
// to every subscriber of every affected workspace in the same instant, and
// every tab answered it at once. It is now answered after a per-tab random
// delay in [0, SYNC_REQUIRED_SPREAD_MS). The lead's ruling on the item's trail
// scopes that to the server signal ONLY: item-change reconciles
// (`items_bulk_updated`, a collection change that rewrote items) are targeted
// freshness and must never wait on the spread timer — the last describe pins
// that, with the spread pending, so a mutant routing them through the timer
// cannot pass by the timer happening to be idle.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { SYNC_REQUIRED_SPREAD_MS, syncRequiredSpreadDelayMs } from './syncSpread';

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
	fireOpen() {
		this.readyState = 1;
		this.onopen?.();
	}
	fire(type: string, data?: unknown) {
		for (const cb of this.listeners.get(type) ?? []) {
			cb(data === undefined ? {} : { data: JSON.stringify(data) });
		}
	}
}

/**
 * `grant` decides whether this tab wins the leader lock. A tab that loses
 * never opens an EventSource and is a pure BroadcastChannel peer.
 */
function installLocks(grant: boolean) {
	Object.defineProperty(globalThis.navigator, 'locks', {
		value: {
			request: (_name: string, _opts: unknown, cb: () => Promise<void>) => {
				if (grant) void cb();
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

/** A connected leader tab with a sync_required counter attached AFTER open. */
async function leader(ws = 'ws-a') {
	const sse = await loadService();
	sse.connect(ws);
	await Promise.resolve();
	await Promise.resolve();
	const source = sources[sources.length - 1];
	source.fireOpen();
	// Registered after the open, so the first-connect sync (BUG-2540, never
	// spread) is not counted.
	const onSync = vi.fn();
	sse.onSyncRequired(onSync);
	// That first-connect sync also set needsSync; clear it so a leg reading
	// needsSync measures the signal under test and not the connect.
	sse.clearSyncFlag();
	return { sse, source, onSync };
}

/**
 * A BroadcastChannel message is delivered a few macrotasks later, and how many
 * is not fixed — so wait for a WITNESS of delivery rather than a tick count.
 * Throws if it never arrives, so a leg asserting "not dispatched yet" cannot
 * pass because the message simply had not landed.
 */
async function until(cond: () => boolean) {
	for (let i = 0; i < 200; i++) {
		if (cond()) return;
		await new Promise<void>((resolve) => setImmediate(resolve));
	}
	throw new Error('BroadcastChannel message never delivered');
}

let channels: BroadcastChannel[] = [];
function channel(ws: string) {
	const bc = new BroadcastChannel(`pad-sync-${ws}`);
	channels.push(bc);
	return bc;
}

beforeEach(() => {
	sources = [];
	vi.stubGlobal('EventSource', FakeEventSource);
	vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
	// Pin the draw: 0.5 → a 2500ms delay.
	vi.spyOn(Math, 'random').mockReturnValue(0.5);
	installLocks(true);
});

afterEach(() => {
	for (const bc of channels) bc.close();
	channels = [];
	vi.useRealTimers();
	vi.restoreAllMocks();
	vi.unstubAllGlobals();
	removeLocks();
});

describe('syncRequiredSpreadDelayMs', () => {
	it('draws from [0, SYNC_REQUIRED_SPREAD_MS)', () => {
		expect(syncRequiredSpreadDelayMs(() => 0)).toBe(0);
		expect(syncRequiredSpreadDelayMs(() => 0.5)).toBe(SYNC_REQUIRED_SPREAD_MS / 2);
		expect(syncRequiredSpreadDelayMs(() => 0.9999999)).toBeLessThan(SYNC_REQUIRED_SPREAD_MS);
	});
});

describe('server sync_required is spread (BUG-2761)', () => {
	it('is not dispatched at once, and is dispatched once at the drawn delay', async () => {
		const { sse, source, onSync } = await leader();

		source.fire('sync_required');
		expect(onSync).not.toHaveBeenCalled();
		// Owed NOW, so a tab resume inside the window still syncs.
		expect(sse.needsSync).toBe(true);

		vi.advanceTimersByTime(2499);
		expect(onSync).not.toHaveBeenCalled();
		vi.advanceTimersByTime(1);
		expect(onSync).toHaveBeenCalledTimes(1);

		vi.advanceTimersByTime(SYNC_REQUIRED_SPREAD_MS);
		expect(onSync).toHaveBeenCalledTimes(1);
		sse.disconnect();
	});

	it('folds signals arriving inside the window into one dispatch, at the FIRST signal\'s delay', async () => {
		// A fold, not a debounce. Restarting the timer on each signal would also
		// produce one dispatch, but under a steady stream of signals less than a
		// window apart it would never fire at all. So the dispatch is read at the
		// first signal's draw (2500ms), with a second signal landing before it.
		const { sse, source, onSync } = await leader();

		source.fire('sync_required');
		vi.advanceTimersByTime(2000);
		source.fire('sync_required');
		source.fire('sync_required');
		vi.advanceTimersByTime(500);
		expect(onSync).toHaveBeenCalledTimes(1);

		vi.advanceTimersByTime(SYNC_REQUIRED_SPREAD_MS * 2);
		expect(onSync).toHaveBeenCalledTimes(1);
		sse.disconnect();
	});

	it('a signal after the window has fired draws a new delay (control for the fold)', async () => {
		const { sse, source, onSync } = await leader();

		source.fire('sync_required');
		vi.advanceTimersByTime(SYNC_REQUIRED_SPREAD_MS);
		expect(onSync).toHaveBeenCalledTimes(1);

		source.fire('sync_required');
		expect(onSync).toHaveBeenCalledTimes(1);
		vi.advanceTimersByTime(SYNC_REQUIRED_SPREAD_MS);
		expect(onSync).toHaveBeenCalledTimes(2);
		sse.disconnect();
	});

	it('a teardown cancels a pending spread, so it cannot fire into the next workspace', async () => {
		const { sse, source, onSync } = await leader('ws-a');

		source.fire('sync_required');
		sse.connect('ws-b');
		await Promise.resolve();
		await Promise.resolve();
		expect(sources).toHaveLength(2);
		// ws-b is not opened: its own first-connect sync would be a second,
		// legitimate dispatch and blur what this leg measures.
		vi.advanceTimersByTime(SYNC_REQUIRED_SPREAD_MS * 2);

		expect(onSync).not.toHaveBeenCalled();
		sse.disconnect();
	});

	it('the leader broadcasts the spread envelope at once, not when its own timer fires', async () => {
		const { sse, source } = await leader();
		const peer = channel('ws-a');
		const seen: unknown[] = [];
		peer.onmessage = (m) => seen.push(m.data);

		source.fire('sync_required');
		// No timer is advanced in this test, so a broadcast sent from the
		// leader's own spread timer could never arrive here.
		await until(() => seen.length > 0);

		expect(seen).toContainEqual({ type: 'sync_required_spread' });
		sse.disconnect();
	});

	it('a peer tab draws its own delay for the spread envelope', async () => {
		installLocks(false);
		const sse = await loadService();
		const onSync = vi.fn();
		sse.onSyncRequired(onSync);
		sse.connect('ws-a');
		await Promise.resolve();
		expect(sources).toHaveLength(0);
		const leaderBc = channel('ws-a');

		leaderBc.postMessage({ type: 'sync_required_spread' });
		// needsSync is the delivery witness: set on receipt, before any timer.
		await until(() => sse.needsSync);
		expect(onSync).not.toHaveBeenCalled();

		vi.advanceTimersByTime(2500);
		expect(onSync).toHaveBeenCalledTimes(1);
		sse.disconnect();
	});

	it('a peer tab still dispatches the plain envelope at once (control)', async () => {
		installLocks(false);
		const sse = await loadService();
		const onSync = vi.fn();
		sse.onSyncRequired(onSync);
		sse.connect('ws-a');
		await Promise.resolve();
		const leaderBc = channel('ws-a');

		leaderBc.postMessage({ type: 'sync_required' });
		// No timer is advanced, so only an immediate dispatch can satisfy this.
		await until(() => onSync.mock.calls.length > 0);

		expect(onSync).toHaveBeenCalledTimes(1);
		sse.disconnect();
	});
});

describe('item-change reconciles are never delayed by the spread timer (lead ruling, BUG-2761)', () => {
	it('items_bulk_updated dispatches at once while a spread is pending, and subsumes it', async () => {
		const { sse, source, onSync } = await leader();

		source.fire('sync_required');
		expect(onSync).not.toHaveBeenCalled();

		source.fire('items_bulk_updated');
		expect(onSync).toHaveBeenCalledTimes(1);

		// The pending spread is covered by the request the bulk reconcile just
		// issued, so it does not fire a second resync for the same gap.
		vi.advanceTimersByTime(SYNC_REQUIRED_SPREAD_MS * 2);
		expect(onSync).toHaveBeenCalledTimes(1);
		sse.disconnect();
	});

	it('a collection change that rewrote items dispatches at once while a spread is pending', async () => {
		const { sse, source, onSync } = await leader();

		source.fire('sync_required');
		source.fire('collection_updated', {
			type: 'collection_updated',
			workspace_id: 'w',
			item_id: '',
			title: '',
			collection: 'tasks',
			collection_id: 'c-1',
			items_changed: true,
			actor: 'u',
			source: 'web',
			timestamp: 0
		});

		expect(onSync).toHaveBeenCalledTimes(1);
		sse.disconnect();
	});

	it('a peer tab dispatches a relayed bulk reconcile at once while a spread is pending', async () => {
		installLocks(false);
		const sse = await loadService();
		const onSync = vi.fn();
		sse.onSyncRequired(onSync);
		sse.connect('ws-a');
		await Promise.resolve();
		const leaderBc = channel('ws-a');

		leaderBc.postMessage({ type: 'sync_required_spread' });
		await until(() => sse.needsSync);
		expect(onSync).not.toHaveBeenCalled();
		leaderBc.postMessage({ type: 'sync_required' });
		await until(() => onSync.mock.calls.length > 0);

		expect(onSync).toHaveBeenCalledTimes(1);
		vi.advanceTimersByTime(SYNC_REQUIRED_SPREAD_MS * 2);
		expect(onSync).toHaveBeenCalledTimes(1);
		sse.disconnect();
	});
});
