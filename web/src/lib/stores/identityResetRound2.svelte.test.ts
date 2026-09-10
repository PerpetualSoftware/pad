import { describe, expect, it, vi, beforeEach } from 'vitest';

/**
 * BUG-3005, codex round 2. Four defects the round-1 fixes left or created.
 *
 * The sharpest is the single-flight slot: dropping a store's DATA on an
 * identity change while leaving the previous identity's promise published made
 * the reset actively harmful, because the next caller joins a request whose own
 * fence then refuses to commit.
 */

const api = vi.hoisted(() => ({
	collections: { list: vi.fn() },
	items: { list: vi.fn(), listByCollection: vi.fn(), get: vi.fn(), starred: vi.fn() },
	workspaces: { list: vi.fn(), get: vi.fn(), me: vi.fn(), create: vi.fn() },
}));
vi.mock('$lib/api/client', () => ({ api }));
vi.mock('./localIndexPersistence', () => ({
	hydrateCollections: vi.fn(async () => null),
	persistCollections: vi.fn(async () => {}),
	readDurableEpoch: vi.fn(async () => null),
}));

const auth = vi.hoisted(() => {
	const listeners = new Set<() => void>();
	let epoch = 0;
	let id = 'user-1';
	return {
		get userId() { return id; },
		setUserId(next: string) { id = next; },
		get identityEpoch() { return epoch; },
		identityFence() {
			const captured = epoch;
			return () => epoch === captured;
		},
		onIdentityChange(fn: () => void) {
			listeners.add(fn);
			return () => listeners.delete(fn);
		},
		fireIdentityChange(nextId?: string) {
			if (nextId !== undefined) id = nextId;
			epoch++;
			for (const fn of listeners) fn();
		},
		resetListeners() {
			listeners.clear();
			epoch = 0;
			id = 'user-1';
		},
	};
});
vi.mock('./auth.svelte', () => ({ authStore: auth }));

beforeEach(() => {
	vi.resetModules();
	auth.resetListeners();
	api.collections.list.mockReset();
	api.items.starred.mockReset();
});

describe('collectionStore single-flight slot', () => {
	it('does not leave B joining A\'s abandoned request', async () => {
		// Without the invalidation: B's `ensureCollections` joins A's published
		// promise, A's identity fence refuses to commit, and B ends with no
		// collections AND no request of its own — a permanently empty sidebar,
		// which is worse than the leak the reset was closing.
		const { collectionStore } = await import('./collections.svelte');
		let resolveA!: (v: unknown) => void;
		api.collections.list.mockReturnValueOnce(new Promise((r) => { resolveA = r; }));
		const loadA = collectionStore.loadCollections('ws');
		// `loadCollections` reads the durable epoch before it fetches, so the
		// request is one microtask away rather than synchronous.
		await Promise.resolve();
		await Promise.resolve();
		// PRECONDITION: A's request is the published one.
		expect(api.collections.list).toHaveBeenCalledTimes(1);

		auth.fireIdentityChange();

		// B asks for a list. It must ISSUE, not join.
		api.collections.list.mockResolvedValueOnce([
			{ id: 'c1', slug: 'tasks', name: 'Tasks', is_default: true, sort_order: 1 },
		]);
		await collectionStore.ensureCollections('ws');

		expect(api.collections.list).toHaveBeenCalledTimes(2);
		expect(collectionStore.collections).toHaveLength(1);
		expect(collectionStore.collectionsAreFreshFor('ws')).toBe(true);

		// And A settling afterwards still commits nothing.
		resolveA([{ id: 'c9', slug: 'alphas', name: 'Alphas', is_default: true, sort_order: 1 }]);
		await loadA;
		expect(collectionStore.collections).toHaveLength(1);
		expect(collectionStore.collections[0].slug).toBe('tasks');
	});
});

describe('adminStore.loadStats', () => {
	it('refuses a response that settles after the identity changed', async () => {
		let resolveFetch!: (v: unknown) => void;
		const fetchMock = vi.fn(() => new Promise((r) => { resolveFetch = r; }));
		vi.stubGlobal('fetch', fetchMock);
		const { adminStore } = await import('./admin.svelte');

		const inflight = adminStore.loadStats();
		// PRECONDITION: the request is actually out.
		expect(fetchMock).toHaveBeenCalledTimes(1);

		auth.fireIdentityChange();
		resolveFetch({ ok: true, json: async () => ({ users: 42 }) });
		await inflight;

		expect(adminStore.stats).toBeNull();
		vi.unstubAllGlobals();
	});
});

describe('toastStore across an identity change', () => {
	it('drops live toasts AND the history, which names items', async () => {
		const { toastStore } = await import('./toast.svelte');
		toastStore.show({ message: 'Archived TASK-12', type: 'success' });
		// PRECONDITION: both surfaces hold it, or the assertion below is about
		// arrays that were always empty.
		expect(toastStore.toasts.length).toBeGreaterThan(0);
		expect(toastStore.history.length).toBeGreaterThan(0);

		auth.fireIdentityChange();

		expect(toastStore.toasts).toHaveLength(0);
		expect(toastStore.history).toHaveLength(0);
		expect(toastStore.unreadCount).toBe(0);
	});
});

describe('workspaceStore single-flight slot across A -> B -> A', () => {
	it('refuses a list issued in the FIRST A session', async () => {
		// Where the sequence bump inside `invalidate` is the only guard left,
		// and the reason it is there rather than just clearing the slot.
		//
		// `loadAll` fences its commit on the captured USER ID, which is the
		// comparison the epoch exists to replace: after A -> B -> A that id
		// agrees, so the only thing standing between A's first-session list and
		// B-then-A's store is `isLatest` — and `isLatest` is a NAVIGATION fence
		// that no identity change moves on its own.
		const { workspaceStore } = await import('./workspace.svelte');
		let resolveA!: (v: unknown) => void;
		api.workspaces.list.mockReturnValueOnce(new Promise((r) => { resolveA = r; }));

		const loadA = workspaceStore.loadAll();
		expect(api.workspaces.list).toHaveBeenCalledTimes(1);

		auth.fireIdentityChange('user-b');
		auth.fireIdentityChange('user-1');

		// A's first-session response arrives last. The captured id matches the
		// current one — this is the same user — and it must still be refused.
		resolveA([{ id: 'w-alpha', slug: 'alphas-private', name: "Alpha's" }]);
		await loadA;

		expect(workspaceStore.workspaces).toHaveLength(0);
	});
});
