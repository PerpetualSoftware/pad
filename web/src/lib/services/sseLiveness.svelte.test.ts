// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`): sse.svelte.ts uses runes.
//
// TASK-2197: a silently dead connection never fires onerror, and the
// keepalive used to be an SSE comment EventSource never surfaces, so "Live"
// stayed up through a dead stream. The service now opens with heartbeat=1,
// treats 75s of silence as death (by comparing the last signal's time with the
// clock, not by counting its own ticks), closes at once on `offline`, and
// reopens on `online` after a 0-2s jitter. Every reconnect arms the catch-up
// delta (BUG-2540), which is what replays the missed window.
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

// Every instance a test loads, so afterEach can retire it (BUG-3508). The
// service adds `online` / `offline` listeners to `window` at module load, and
// each test's vi.resetModules() import adds another set to the SAME window.
// A test that left its instance connected (the heartbeat leg did) kept it
// listening, so a later test's `offline` + `online` reopened ITS stream too,
// through the stub EventSource, into the shared `sources`: 3 where 2 were
// expected. Order-dependent, not load-dependent: --sequence.shuffle
// reproduced it at once.
const loaded: Array<{ disconnect(): void }> = [];

async function loadService() {
	vi.resetModules();
	const sse = (await import('./sse.svelte')).sseService;
	loaded.push(sse);
	return sse;
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

beforeEach(() => {
	sources = [];
	vi.useFakeTimers();
	vi.stubGlobal('EventSource', FakeEventSource);
	installLocks();
});

afterEach(() => {
	// Disconnected, an instance's window listeners return at once.
	for (const sse of loaded.splice(0)) sse.disconnect();
	vi.useRealTimers();
	vi.unstubAllGlobals();
	vi.restoreAllMocks();
	removeLocks();
});

describe('SSE liveness (TASK-2197)', () => {
	it('opens with heartbeat=1', async () => {
		const sse = await connected();
		expect(sources[0].url).toContain('heartbeat=1');
		sse.disconnect();
	});

	it('heartbeats keep a quiet stream connected', async () => {
		const sse = await connected();
		for (let i = 0; i < 10; i++) {
			await vi.advanceTimersByTimeAsync(30_000);
			sources[0].fire('heartbeat');
		}
		expect(sse.status).toBe('connected');
		expect(sources).toHaveLength(1);
		sse.disconnect();
	});

	it('75s of silence drops Live, reconnects, and the reconnect arms the catch-up sync', async () => {
		const sse = await connected();
		const synced = vi.fn();
		sse.onSyncRequired(synced);
		await vi.advanceTimersByTimeAsync(76_000 + 15_000); // past the bound, plus one watchdog period
		expect(sse.status).toBe('reconnecting');
		expect(sources[0].closed).toBe(true);
		await vi.advanceTimersByTimeAsync(5 * 60_000); // the reconnect ladder
		expect(sources.length).toBeGreaterThan(1);
		synced.mockClear();
		sources[sources.length - 1].fireOpen();
		await vi.advanceTimersByTimeAsync(10_000); // past any sync spread
		expect(synced).toHaveBeenCalled();
		sse.disconnect();
	});

	// The watchdog's own tick is captured and run by hand, so "late" is real:
	// 120s pass with no tick at all, then one tick runs.
	async function connectedCapturingWatchdog() {
		const ticks: Array<() => void> = [];
		vi.spyOn(globalThis, 'setInterval').mockImplementation(((cb: () => void) => {
			ticks.push(cb);
			return 0 as unknown as ReturnType<typeof setInterval>;
		}) as typeof setInterval);
		const sse = await connected();
		expect(ticks).toHaveLength(1);
		return { sse, tick: ticks[0] };
	}

	it('a watchdog tick that runs LATE does not trip while events kept arriving (a throttled background tab)', async () => {
		const { sse, tick } = await connectedCapturingWatchdog();
		for (let i = 0; i < 6; i++) {
			vi.setSystemTime(Date.now() + 20_000);
			sources[0].fire('heartbeat');
		}
		tick(); // 120s after the open, but 0s after the last heartbeat
		expect(sse.status).toBe('connected');
		expect(sources[0].closed).toBe(false);
		sse.disconnect();
	});

	it('CONTROL: the same late tick with no events in between does trip', async () => {
		const { sse, tick } = await connectedCapturingWatchdog();
		vi.setSystemTime(Date.now() + 120_000);
		tick();
		expect(sse.status).toBe('reconnecting');
		sse.disconnect();
	});

	it('the browser going offline drops Live at once and closes the stream', async () => {
		const sse = await connected();
		window.dispatchEvent(new Event('offline'));
		expect(sse.status).toBe('reconnecting');
		expect(sources[0].closed).toBe(true);
		sse.disconnect();
	});

	it('coming back online reopens within the 0-2s jitter, with the catch-up armed', async () => {
		const sse = await connected();
		const synced = vi.fn();
		sse.onSyncRequired(synced);
		window.dispatchEvent(new Event('offline'));
		vi.spyOn(Math, 'random').mockReturnValue(0.999);
		window.dispatchEvent(new Event('online'));
		await vi.advanceTimersByTimeAsync(1_000);
		expect(sources).toHaveLength(1); // jittered: not at once
		await vi.advanceTimersByTimeAsync(1_100);
		expect(sources).toHaveLength(2);
		synced.mockClear();
		sources[1].fireOpen();
		await vi.advanceTimersByTimeAsync(10_000);
		expect(synced).toHaveBeenCalled();
		sse.disconnect();
	});

	it('a source due while the browser is offline is not opened; online opens it (codex r1)', async () => {
		const online = vi.spyOn(navigator, 'onLine', 'get').mockReturnValue(false);
		const sse = await loadService();
		sse.connect('ws-a'); // e.g. a follower promoted mid-outage
		await flush();
		expect(sources).toHaveLength(0);
		expect(sse.status).toBe('reconnecting');
		online.mockReturnValue(true);
		vi.spyOn(Math, 'random').mockReturnValue(0);
		window.dispatchEvent(new Event('online'));
		await vi.advanceTimersByTimeAsync(10);
		expect(sources).toHaveLength(1);
		sse.disconnect();
	});

	it('online with nothing closed for offline opens nothing', async () => {
		const sse = await connected();
		window.dispatchEvent(new Event('online'));
		await vi.advanceTimersByTimeAsync(3_000);
		expect(sources).toHaveLength(1);
		sse.disconnect();
	});
});
