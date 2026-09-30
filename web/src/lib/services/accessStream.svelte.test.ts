import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// TASK-3275: the workspace-access stream (one per browser since BUG-3318; these
// legs run without navigator.locks, the per-tab fallback). Every collaborator is a
// fake, so each test asserts what the service DID: which reads it asked for,
// where it navigated, how many sources it opened.

const h = vi.hoisted(() => {
	type Tab = { slug: string; owner_username: string };
	const state = {
		epoch: 0,
		authenticated: true,
		identityListeners: [] as Array<(prev: string) => void>,
		tabs: [] as Tab[],
		// What the NEXT tabsStore.load() will commit.
		nextTabs: null as Tab[] | null,
		current: null as { id: string; slug: string } | null,
		params: {} as Record<string, string>
	};
	return {
		state,
		goto: vi.fn(async () => {}),
		loadAll: vi.fn(async () => {}),
		load: vi.fn(async () => {
			if (state.nextTabs) state.tabs = state.nextTabs;
		}),
		highlight: vi.fn(),
		// GET /workspaces/{slug}: resolves = still reachable.
		getWs: vi.fn(async () => ({}))
	};
});

const GONE = Object.assign(new Error('not found'), { code: 'not_found', details: { scope: 'workspace' } });
const goneFor = () => h.getWs.mockRejectedValue(GONE);

vi.mock('$app/navigation', () => ({ goto: h.goto }));
vi.mock('$lib/api/client', async () => {
	const actual = await vi.importActual<typeof import('$lib/api/client')>('$lib/api/client');
	return { api: { workspaces: { get: h.getWs } }, isWorkspaceNotFoundBody: actual.isWorkspaceNotFoundBody };
});
vi.mock('$app/state', () => ({ page: { get params() { return h.state.params; } } }));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get authenticated() { return h.state.authenticated; },
		identityFence() {
			const at = h.state.epoch;
			return () => h.state.epoch === at;
		},
		onIdentityChange(fn: (prev: string) => void) {
			h.state.identityListeners.push(fn);
			return () => {};
		}
	}
}));
vi.mock('$lib/stores/tabs.svelte', () => ({
	tabsStore: { get tabs() { return h.state.tabs; }, load: h.load }
}));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { get current() { return h.state.current; }, loadAll: h.loadAll }
}));
vi.mock('$lib/stores/ui.svelte', () => ({ uiStore: { highlightAddWorkspace: h.highlight } }));
vi.mock('$lib/utils/workspace-route', () => ({
	workspaceRestoreTarget: (ws: { slug: string; owner_username?: string }) => `/${ws.owner_username}/${ws.slug}`
}));

const sources: FakeEventSource[] = [];

class FakeEventSource {
	static readonly CONNECTING = 0;
	static readonly OPEN = 1;
	static readonly CLOSED = 2;
	url: string;
	readyState = 0;
	onerror: (() => void) | null = null;
	closed = false;
	private listeners = new Map<string, Set<(e: unknown) => void>>();
	constructor(url: string) {
		this.url = url;
		sources.push(this);
	}
	addEventListener(type: string, cb: (e: unknown) => void) {
		let set = this.listeners.get(type);
		if (!set) this.listeners.set(type, (set = new Set()));
		set.add(cb);
	}
	close() {
		this.closed = true;
		this.readyState = FakeEventSource.CLOSED;
	}
	fireDropped() {
		this.readyState = FakeEventSource.CONNECTING;
		this.onerror?.();
	}
	fire(type: string, data?: unknown) {
		for (const cb of this.listeners.get(type) ?? []) cb(data === undefined ? {} : { data: JSON.stringify(data) });
	}
}

const tab = (slug: string) => ({ slug, owner_username: 'dave' });
const hint = (change: string, workspace_id = 'ws-b') => ({ kind: 'workspace_access_changed', workspace_id, change });

async function flush() {
	for (let i = 0; i < 5; i++) await Promise.resolve();
}

async function freshStream() {
	vi.resetModules();
	const mod = await import('./accessStream.svelte');
	return mod.accessStream;
}

function changeIdentity(authenticated = true) {
	h.state.epoch++;
	h.state.authenticated = authenticated;
	for (const fn of h.state.identityListeners) fn('previous-user');
}

beforeEach(() => {
	sources.length = 0;
	vi.useFakeTimers();
	vi.stubGlobal('EventSource', FakeEventSource);
	Object.assign(h.state, {
		epoch: 0,
		authenticated: true,
		identityListeners: [],
		tabs: [tab('a'), tab('b'), tab('c')],
		nextTabs: null,
		current: { id: 'ws-b', slug: 'b' },
		params: { username: 'dave', workspace: 'b' }
	});
	h.goto.mockClear();
	h.load.mockClear();
	h.loadAll.mockClear();
	h.highlight.mockClear();
	h.getWs.mockReset();
	h.getWs.mockResolvedValue({});
});

afterEach(() => {
	vi.useRealTimers();
	vi.unstubAllGlobals();
});

describe('accessStream (TASK-3275)', () => {
	it('opens ONE source per tab on the opt-in URL, however often start() is called', async () => {
		const s = await freshStream();
		s.start();
		s.start();
		s.start();
		expect(sources).toHaveLength(1);
		expect(sources[0].url).toBe('/api/v1/events/stream?access=true');
	});

	it('refetches the open set and the workspace list on a hint, and applies nothing from it', async () => {
		const s = await freshStream();
		s.start();
		sources[0].fire('notification', hint('gained', 'ws-new'));
		await flush();
		expect(h.load).toHaveBeenCalledTimes(1);
		expect(h.loadAll).toHaveBeenCalledTimes(1);
		expect(h.goto).not.toHaveBeenCalled();
	});

	it('ignores every other notification kind', async () => {
		const s = await freshStream();
		s.start();
		sources[0].fire('notification', { kind: 'status-change', item_ref: 'TASK-1' });
		await flush();
		expect(h.load).not.toHaveBeenCalled();
	});

	it('drops a hint that arrives after an identity change', async () => {
		const s = await freshStream();
		s.start();
		const old = sources[0];
		changeIdentity();
		old.fire('notification', hint('lost'));
		await flush();
		expect(h.load).not.toHaveBeenCalled();
		expect(old.closed).toBe(true);
		// Reopened for whoever is signed in now.
		expect(sources).toHaveLength(2);
	});

	it('does not navigate when the identity changes while the refetch is in flight', async () => {
		goneFor();
		const s = await freshStream();
		s.start();
		h.state.nextTabs = [tab('a'), tab('c')];
		h.load.mockImplementationOnce(async () => {
			changeIdentity();
			h.state.tabs = h.state.nextTabs!;
		});
		sources[0].fire('notification', hint('lost'));
		await flush();
		// Nor asks the server about a workspace this tab has moved past.
		expect(h.getWs).not.toHaveBeenCalled();
		expect(h.goto).not.toHaveBeenCalled();
	});

	it('stays closed after sign-out', async () => {
		const s = await freshStream();
		s.start();
		changeIdentity(false);
		expect(sources).toHaveLength(1);
		expect(sources[0].closed).toBe(true);
	});

	it('lands on the left neighbour when the ACTIVE workspace is lost', async () => {
		goneFor();
		const s = await freshStream();
		s.start();
		h.state.nextTabs = [tab('a'), tab('c')];
		sources[0].fire('notification', hint('lost'));
		await flush();
		expect(h.goto).toHaveBeenCalledWith('/dave/a');
	});

	it('lands on /console when the lost active workspace was the only tab', async () => {
		h.state.tabs = [tab('b')];
		goneFor();
		const s = await freshStream();
		s.start();
		h.state.nextTabs = [];
		sources[0].fire('notification', hint('deleted'));
		await flush();
		expect(h.goto).toHaveBeenCalledWith('/console');
		expect(h.highlight).toHaveBeenCalled();
	});

	it('does not move the tab when a DIFFERENT workspace is lost', async () => {
		// The active tab's row is gone too (closed on another device), so only
		// the hint's workspace_id stands between this and a navigation.
		goneFor();
		const s = await freshStream();
		s.start();
		h.state.nextTabs = [tab('c')];
		sources[0].fire('notification', hint('lost', 'ws-a'));
		await flush();
		expect(h.load).toHaveBeenCalledTimes(1);
		expect(h.goto).not.toHaveBeenCalled();
	});

	it('does not move the tab when the server says the workspace still resolves (a stale hint)', async () => {
		// The tab row is gone too (closed on another device), so only the
		// access probe stands between this and a navigation (codex round 1).
		const s = await freshStream();
		s.start();
		h.state.nextTabs = [tab('a'), tab('c')];
		sources[0].fire('notification', hint('lost'));
		await flush();
		expect(h.getWs).toHaveBeenCalledWith('b');
		expect(h.goto).not.toHaveBeenCalled();
	});

	it('does not move the tab when the probe fails for any other reason', async () => {
		h.getWs.mockRejectedValue(Object.assign(new Error('boom'), { code: 'internal_error' }));
		const s = await freshStream();
		s.start();
		h.state.nextTabs = [tab('a'), tab('c')];
		sources[0].fire('notification', hint('lost'));
		await flush();
		expect(h.goto).not.toHaveBeenCalled();
	});

	it('does not move the tab when the user left the workspace while the refetch was in flight', async () => {
		goneFor();
		const s = await freshStream();
		s.start();
		h.state.nextTabs = [tab('a'), tab('c')];
		h.load.mockImplementationOnce(async () => {
			h.state.params = { username: 'dave', workspace: 'c' };
			h.state.tabs = h.state.nextTabs!;
		});
		sources[0].fire('notification', hint('lost'));
		await flush();
		// Nor asks the server about a workspace this tab has moved past.
		expect(h.getWs).not.toHaveBeenCalled();
		expect(h.goto).not.toHaveBeenCalled();
	});

	it('does not move the tab when the user left the workspace while the probe was in flight', async () => {
		h.getWs.mockImplementationOnce(async () => {
			h.state.params = { username: 'dave', workspace: 'c' };
			throw GONE;
		});
		const s = await freshStream();
		s.start();
		h.state.nextTabs = [tab('a'), tab('c')];
		sources[0].fire('notification', hint('lost'));
		await flush();
		expect(h.goto).not.toHaveBeenCalled();
	});

	it('does not move the tab on a gain, even when the tab row went away', async () => {
		// A tab closed on another device is not a lost workspace.
		goneFor();
		const s = await freshStream();
		s.start();
		h.state.nextTabs = [tab('a'), tab('c')];
		sources[0].fire('notification', hint('gained'));
		await flush();
		expect(h.goto).not.toHaveBeenCalled();
	});

	it('refetches once on sync_required and once on a reconnect, not on the first connect', async () => {
		const s = await freshStream();
		s.start();
		sources[0].fire('connected');
		await flush();
		expect(h.load).not.toHaveBeenCalled();

		sources[0].fire('sync_required');
		await flush();
		expect(h.load).toHaveBeenCalledTimes(1);

		sources[0].fireDropped();
		await vi.runOnlyPendingTimersAsync();
		expect(sources).toHaveLength(2);
		sources[1].fire('connected');
		await flush();
		expect(h.load).toHaveBeenCalledTimes(2);
	});

	it('stops for good on unauthorized', async () => {
		const s = await freshStream();
		s.start();
		sources[0].fire('unauthorized');
		await vi.runOnlyPendingTimersAsync();
		expect(sources).toHaveLength(1);
		expect(sources[0].closed).toBe(true);
		expect(s.connected).toBe(false);
	});
});

// BUG-3318: over HTTP/1.1 a browser allows 6 connections per host, and one
// stream per tab starved new pages. One tab per browser holds the stream and
// relays; every tab still acts for itself. Two freshStream() calls are two
// tabs: separate module instances sharing the fakes below.
describe('accessStream election (BUG-3318)', () => {
	type Waiter = { name: string; run: () => unknown };
	let held: Set<string>;
	let queue: Waiter[];
	let channels: Array<{ name: string; onmessage: ((e: { data: unknown }) => void) | null; closed: boolean }>;

	function grantNext(name: string) {
		const i = queue.findIndex((w) => w.name === name);
		if (i === -1) return;
		const [w] = queue.splice(i, 1);
		held.add(name);
		void Promise.resolve(w.run()).then(() => {
			held.delete(name);
			grantNext(name);
		});
	}

	beforeEach(() => {
		held = new Set();
		queue = [];
		channels = [];
		const locks = {
			request: (name: string, _opts: unknown, cb: () => unknown) =>
				new Promise<void>((resolve) => {
					queue.push({ name, run: async () => { await cb(); resolve(); } });
					if (!held.has(name)) grantNext(name);
				}),
			query: async () => ({ held: [...held].map((name) => ({ name })) })
		};
		Object.defineProperty(navigator, 'locks', { value: locks, configurable: true });
		class FakeChannel {
			name: string;
			onmessage: ((e: { data: unknown }) => void) | null = null;
			closed = false;
			constructor(name: string) {
				this.name = name;
				channels.push(this);
			}
			postMessage(data: unknown) {
				for (const c of channels) {
					if (c !== this && !c.closed && c.name === this.name) queueMicrotask(() => c.onmessage?.({ data }));
				}
			}
			close() {
				this.closed = true;
			}
		}
		vi.stubGlobal('BroadcastChannel', FakeChannel);
	});

	afterEach(() => {
		Object.defineProperty(navigator, 'locks', { value: undefined, configurable: true });
	});

	it('two tabs hold ONE stream, and a hint makes both refetch', async () => {
		const a = await freshStream();
		const b = await freshStream();
		a.start();
		await flush();
		b.start();
		await flush();
		expect(sources.filter((s) => !s.closed)).toHaveLength(1);
		sources[0].fire('connected');
		await flush();
		expect(h.load).not.toHaveBeenCalled();

		sources[0].fire('notification', hint('gained'));
		await flush();
		// Leader and follower each refetched for their own route.
		expect(h.load).toHaveBeenCalledTimes(2);
	});

	it('a follower takes over when the leader goes, and its connect resyncs every tab', async () => {
		const a = await freshStream();
		const b = await freshStream();
		const c = await freshStream();
		a.start();
		await flush();
		b.start();
		c.start();
		await flush();
		expect(sources).toHaveLength(1);

		a.stop();
		await flush();
		expect(sources[0].closed).toBe(true);
		expect(sources).toHaveLength(2);
		expect(sources.filter((s) => !s.closed)).toHaveLength(1);

		// The takeover's connect is a reconnect: the new leader and the
		// remaining follower both resync, covering a hint fired in the gap.
		h.load.mockClear();
		sources[1].fire('connected');
		await flush();
		expect(h.load).toHaveBeenCalledTimes(2);

		sources[1].fire('notification', hint('gained'));
		await flush();
		expect(h.load).toHaveBeenCalledTimes(4);
	});

	it("the first leader's first connect is not a resync", async () => {
		const a = await freshStream();
		a.start();
		await flush();
		sources[0].fire('connected');
		await flush();
		expect(h.load).not.toHaveBeenCalled();
	});

	it('a stopped tab neither streams nor hears relayed hints', async () => {
		const a = await freshStream();
		const b = await freshStream();
		a.start();
		await flush();
		b.start();
		await flush();
		b.stop();
		await flush();
		sources[0].fire('notification', hint('gained'));
		await flush();
		expect(h.load).toHaveBeenCalledTimes(1);
	});
});
