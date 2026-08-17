// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`) because
// sse.svelte.ts uses runes.
//
// WHAT THIS PINS (BUG-2611). close() does not retract already-queued event
// tasks, so on a fast workspace switch a torn-down source's queued events can
// fire after the next workspace's source exists. BUG-2540 guarded onopen /
// onerror / `connected` with a `source === eventSource` identity check; the
// remaining listeners (`sync_required`, `items_bulk_updated`, `unauthorized`,
// and the ITEM_EVENTS loop) had none — so workspace A's stale events
// dispatched into workspace B's callbacks, and a stale `unauthorized` (the
// sharp member) closed B's LIVE EventSource, flipped the status indicator,
// and cleared the current workspace over A's auth state.
//
// Every leg here has a control arm: the same event fired on the LIVE source
// must still dispatch — a guard that silences both would pass a guards-only
// assertion while breaking the product.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

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
	/**
	 * Fire a named event exactly as the browser would deliver a QUEUED task
	 * from this source — including after close(), which is the whole bug.
	 */
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

/**
 * Drive a fast A→B workspace switch and return both sources. A is torn
 * down (closed) but retains its queued-task ability via fire().
 */
async function switchedService() {
	const sse = await loadService();
	sse.connect('ws-a');
	await Promise.resolve();
	await Promise.resolve();
	expect(sources).toHaveLength(1);
	sse.connect('ws-b');
	await Promise.resolve();
	await Promise.resolve();
	expect(sources).toHaveLength(2);
	const [a, b] = sources;
	expect(a.closed).toBe(true);
	b.fireOpen();
	return { sse, a, b };
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

describe('SSE stale-source guards (BUG-2611)', () => {
	it('drops a stale sync_required; the live source still dispatches (control)', async () => {
		const { sse, a, b } = await switchedService();
		const onSync = vi.fn();
		sse.onSyncRequired(onSync);

		a.fire('sync_required');
		expect(onSync).not.toHaveBeenCalled();

		b.fire('sync_required');
		expect(onSync).toHaveBeenCalledTimes(1);
		sse.disconnect();
	});

	it('drops a stale items_bulk_updated; the live source still dispatches (control)', async () => {
		const { sse, a, b } = await switchedService();
		const onSync = vi.fn();
		sse.onSyncRequired(onSync);

		a.fire('items_bulk_updated');
		expect(onSync).not.toHaveBeenCalled();

		b.fire('items_bulk_updated');
		expect(onSync).toHaveBeenCalledTimes(1);
		sse.disconnect();
	});

	it('drops a stale item event; the live source still dispatches (control)', async () => {
		const { sse, a, b } = await switchedService();
		const onItem = vi.fn();
		sse.onItemEvent(onItem);

		a.fire('item_updated', { type: 'item_updated', item_id: 'i-1', workspace: 'ws-a' });
		expect(onItem).not.toHaveBeenCalled();

		b.fire('item_updated', { type: 'item_updated', item_id: 'i-2', workspace: 'ws-b' });
		expect(onItem).toHaveBeenCalledTimes(1);
		expect(onItem.mock.calls[0][0].item_id).toBe('i-2');
		sse.disconnect();
	});

	it("a stale unauthorized cannot tear down the NEW workspace's connection", async () => {
		const { sse, a, b } = await switchedService();
		expect(sse.status).toBe('connected');

		a.fire('unauthorized');
		// The live source survives, the status stays the live source's, and
		// the service still considers itself connected to ws-b.
		expect(b.closed).toBe(false);
		expect(sse.status).toBe('connected');

		// CONTROL: the live source's own unauthorized still acts.
		b.fire('unauthorized');
		expect(b.closed).toBe(true);
		expect(sse.status).toBe('unauthorized');
		sse.disconnect();
	});
});
