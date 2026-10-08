// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`): sse.svelte.ts uses runes.
//
// BUG-3320: over HTTP/1.1 a browser allows 6 connections per host, and every
// live stream holds one. Measured: 5 workspaces open in 5 tabs filled the
// pool and the next tab could not load. On an HTTP/1.1 page a workspace leader
// now needs one of STREAM_SLOTS browser-wide slots; without one it polls the
// /items-changes reconcile and retries for a slot each time. h2/h3 take no
// slot. The browser-level repro is e2e/bug-3320-stream-budget.spec.ts.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

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
	constructor(url: string) {
		this.url = url;
		sources.push(this);
	}
	private listeners = new Map<string, Array<() => void>>();
	addEventListener(type: string, cb: () => void) {
		const l = this.listeners.get(type) ?? [];
		l.push(cb);
		this.listeners.set(type, l);
	}
	fire(type: string) {
		for (const cb of this.listeners.get(type) ?? []) cb();
	}
	close() {
		this.closed = true;
		this.readyState = FakeEventSource.CLOSED;
	}
	fireOpen() {
		this.readyState = 1;
		this.onopen?.();
	}
}

// navigator.locks with the two behaviours the service uses: an exclusive
// request that waits, and ifAvailable, which answers null when held.
const held = new Set<string>();
const queues = new Map<string, Array<() => void>>();
function grant(name: string, cb: (lock: unknown) => unknown): Promise<unknown> {
	held.add(name);
	return Promise.resolve(cb({ name })).finally(() => {
		held.delete(name);
		const next = queues.get(name)?.shift();
		next?.();
	});
}
function installLocks() {
	held.clear();
	queues.clear();
	Object.defineProperty(globalThis.navigator, 'locks', {
		value: {
			request: (name: string, opts: { ifAvailable?: boolean }, cb: (lock: unknown) => unknown) => {
				if (opts?.ifAvailable) {
					if (held.has(name)) return Promise.resolve(cb(null));
					return grant(name, cb);
				}
				if (!held.has(name)) return grant(name, cb);
				return new Promise((resolve) => {
					const q = queues.get(name) ?? [];
					q.push(() => resolve(grant(name, cb)));
					queues.set(name, q);
				});
			},
			query: async () => ({ held: [], pending: [] })
		},
		configurable: true,
		writable: true
	});
}

/** Another tab holds stream slot i until the returned function is called. */
function holdSlot(i: number): () => void {
	let release!: () => void;
	void navigator.locks.request(`pad-sse-slot-${i}`, { ifAvailable: true }, () => new Promise<void>((r) => (release = r)));
	return () => release();
}

function setProtocol(p: string | null) {
	vi.spyOn(performance, 'getEntriesByType').mockImplementation(((type: string) =>
		type === 'navigation' && p !== null ? [{ nextHopProtocol: p }] : []) as typeof performance.getEntriesByType);
}

async function loadService() {
	vi.resetModules();
	const mod = await import('./sse.svelte');
	return { svc: mod.sseService, POLL_EVERY_MS: mod.POLL_EVERY_MS };
}

async function flush() {
	for (let i = 0; i < 20; i++) await Promise.resolve();
}

describe('BUG-3320: the HTTP/1.1 stream budget', () => {
	beforeEach(() => {
		sources = [];
		vi.stubGlobal('EventSource', FakeEventSource);
		vi.stubGlobal('BroadcastChannel', undefined);
		installLocks();
	});
	afterEach(() => {
		vi.useRealTimers();
		vi.unstubAllGlobals();
		vi.restoreAllMocks();
	});

	it('on HTTP/1.1 a leader with a free slot streams, and holds the slot until it disconnects', async () => {
		setProtocol('http/1.1');
		const { svc } = await loadService();
		svc.connect('ws-a');
		await flush();
		expect(sources).toHaveLength(1);
		expect(held.has('pad-sse-slot-0')).toBe(true);
		svc.disconnect();
		await flush();
		expect(held.has('pad-sse-slot-0')).toBe(false);
	});

	it('on HTTP/1.1 with every slot held elsewhere, it opens NO stream and polls instead', async () => {
		vi.useFakeTimers();
		setProtocol('http/1.1');
		const others = [holdSlot(0), holdSlot(1), holdSlot(2)];
		const { svc, POLL_EVERY_MS } = await loadService();
		let syncs = 0;
		svc.onSyncRequired(() => syncs++);
		svc.connect('ws-d');
		await flush();
		expect(sources).toHaveLength(0);
		expect(svc.status).toBe('polling');
		expect(syncs).toBe(1); // the first-connect gap, covered at once

		await vi.advanceTimersByTimeAsync(POLL_EVERY_MS);
		await flush();
		expect(syncs).toBe(2);
		expect(sources).toHaveLength(0);

		// A slot frees (another workspace's tab closed): the next tick streams.
		others[1]();
		await flush();
		await vi.advanceTimersByTimeAsync(POLL_EVERY_MS);
		await flush();
		expect(sources).toHaveLength(1);
		expect(held.has('pad-sse-slot-1')).toBe(true);
		sources[0].fireOpen();
		expect(svc.status).toBe('connected');
		svc.disconnect();
	});

	it('a layout re-run on the same workspace leaves a polling leader alone', async () => {
		// A real BroadcastChannel: with none, connect() never reaches the
		// teardown this no-op guards against.
		vi.stubGlobal(
			'BroadcastChannel',
			class {
				onmessage: unknown = null;
				postMessage() {}
				close() {}
			}
		);
		vi.useFakeTimers();
		setProtocol('http/1.1');
		holdSlot(0);
		holdSlot(1);
		holdSlot(2);
		const { svc } = await loadService();
		let syncs = 0;
		svc.onSyncRequired(() => syncs++);
		svc.connect('ws-d');
		await flush();
		expect(svc.status).toBe('polling');
		expect(syncs).toBe(1);
		svc.connect('ws-d');
		await flush();
		// Torn down and re-entered, it would have dispatched a second
		// immediate sync (and, in a browser, dropped and re-won leadership).
		expect(syncs).toBe(1);
		expect(svc.status).toBe('polling');
		expect(svc.isLeader).toBe(true);
		svc.disconnect();
	});

	it('disconnect stops the poll', async () => {
		vi.useFakeTimers();
		setProtocol('http/1.1');
		holdSlot(0);
		holdSlot(1);
		holdSlot(2);
		const { svc, POLL_EVERY_MS } = await loadService();
		let syncs = 0;
		svc.onSyncRequired(() => syncs++);
		svc.connect('ws-d');
		await flush();
		svc.disconnect();
		const before = syncs;
		await vi.advanceTimersByTimeAsync(POLL_EVERY_MS * 3);
		expect(syncs).toBe(before);
	});

	it('a disconnect and same-workspace reconnect during the slot request leaves one leader and one stream (codex r1)', async () => {
		setProtocol('http/1.1');
		const { svc } = await loadService();
		svc.connect('ws-a'); // the lock callback runs and awaits its slot
		svc.disconnect();
		svc.connect('ws-a'); // queued behind the first callback's lock
		await flush();
		const live = sources.filter((s) => !s.closed);
		expect(live).toHaveLength(1);
		expect(svc.isLeader).toBe(true);
		expect([0, 1, 2].filter((i) => held.has(`pad-sse-slot-${i}`))).toHaveLength(1);
		svc.disconnect();
		await flush();
		expect([0, 1, 2].filter((i) => held.has(`pad-sse-slot-${i}`))).toHaveLength(0);
	});

	it('losing access frees the slot (codex r1)', async () => {
		setProtocol('http/1.1');
		const { svc } = await loadService();
		svc.connect('ws-a');
		await flush();
		expect(held.has('pad-sse-slot-0')).toBe(true);
		sources[0].fire('unauthorized');
		await flush();
		expect(svc.status).toBe('unauthorized');
		expect(held.has('pad-sse-slot-0')).toBe(false);
	});

	for (const protocol of ['h2', 'h3', '']) {
		it(`on ${protocol || 'an unreported protocol'} every tab streams, slot or not, and takes no slot`, async () => {
			setProtocol(protocol);
			holdSlot(0);
			holdSlot(1);
			holdSlot(2);
			const { svc } = await loadService();
			svc.connect('ws-a');
			await flush();
			expect(sources).toHaveLength(1);
			expect(svc.status).not.toBe('polling');
			svc.disconnect();
		});
	}
});
