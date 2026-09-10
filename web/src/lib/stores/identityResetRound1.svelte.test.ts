import { describe, expect, it, vi, beforeEach } from 'vitest';

/**
 * BUG-3005, codex round 1. The stores round 1 found still holding per-user
 * state, plus the unfenced `loading` write in collectionStore.
 *
 * The four stores named on the item were not the population — the enumeration
 * that produced them swept for per-user FIELDS, and these hold user-scoped
 * state that does not look like one: an authorization-scoped statistics blob,
 * a browser-tab title retired by a PATH stamp that an account swap does not
 * move, and editor scalars that are consumed by GUARDS rather than displayed.
 */

const api = vi.hoisted(() => ({
	collections: { list: vi.fn() },
	items: { list: vi.fn(), listByCollection: vi.fn(), get: vi.fn() },
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
	return {
		userId: 'user-1',
		get identityEpoch() { return epoch; },
		identityFence() {
			const captured = epoch;
			return () => epoch === captured;
		},
		onIdentityChange(fn: () => void) {
			listeners.add(fn);
			return () => listeners.delete(fn);
		},
		fireIdentityChange() {
			epoch++;
			for (const fn of listeners) fn();
		},
		resetListeners() {
			listeners.clear();
			epoch = 0;
		},
	};
});
vi.mock('./auth.svelte', () => ({ authStore: auth }));

beforeEach(() => {
	vi.resetModules();
	auth.resetListeners();
	api.collections.list.mockReset();
	api.items.list.mockReset();
});

describe('collectionStore loading flag', () => {
	it('is not cleared by a load that settles after the identity changed', async () => {
		// `loading` is module state written after an await like any other. A
		// settling as B's load is still in flight clears B's flag and flashes
		// empty-state or error UI mid-load.
		const { collectionStore } = await import('./collections.svelte');
		let resolveA!: (v: unknown) => void;
		api.items.list.mockReturnValueOnce(new Promise((r) => { resolveA = r; }));
		const loadA = collectionStore.loadItems('ws');
		expect(collectionStore.loading).toBe(true);

		auth.fireIdentityChange();
		// PRECONDITION: the identity reset owns the flag, so nothing is stuck.
		expect(collectionStore.loading).toBe(false);

		// B starts its own load, then A settles underneath it.
		api.items.list.mockReturnValueOnce(new Promise(() => {}));
		void collectionStore.loadItems('ws');
		expect(collectionStore.loading).toBe(true);

		resolveA([]);
		await loadA;

		expect(collectionStore.loading).toBe(true);
	});
});

describe('adminStore across an identity change', () => {
	it('drops the previous admin\'s statistics', async () => {
		// `stats` is REALLY loaded first, through a stubbed fetch. Asserting
		// null before and after would pass against a store with no clear at
		// all — the failure mode this whole file exists to avoid.
		const fetchMock = vi.fn(async () => ({
			ok: true,
			json: async () => ({ users: 42 }),
		}));
		vi.stubGlobal('fetch', fetchMock);
		const { adminStore } = await import('./admin.svelte');
		await adminStore.loadStats();

		// PRECONDITION: the statistics are actually present.
		expect(adminStore.stats).not.toBeNull();
		expect(adminStore.loading).toBe(false);

		auth.fireIdentityChange();

		expect(adminStore.stats).toBeNull();
		expect(adminStore.loading).toBe(true);
		expect(adminStore.error).toBe('');
		vi.unstubAllGlobals();
	});
});

describe('titleStore across an identity change', () => {
	it('drops the previous user\'s workspace and item title', async () => {
		const { titleStore } = await import('./title.svelte');
		titleStore.setPageTitle({ workspace: 'Acme', section: 'Ideas', item: 'IDEA-1' });
		// PRECONDITION: the workspace part is set — it is the unstamped one, so
		// it is the part a route change would NOT retire.
		expect(titleStore.workspace).toBe('Acme');

		auth.fireIdentityChange();

		expect(titleStore.workspace).toBeUndefined();
		expect(titleStore.section).toBeUndefined();
		expect(titleStore.item).toBeUndefined();
	});
});

describe('editorStore across an identity change', () => {
	it('drops dirty and saveStatus, which are read by GUARDS', async () => {
		const { editorStore } = await import('./editor.svelte');
		editorStore.setDirty(true);
		editorStore.setSaveStatus('saving');
		editorStore.setMode('raw');
		editorStore.setLastSaveTime(1234);
		editorStore.setExternalChange(true);
		// PRECONDITION: all five are set, so the assertion below is about the
		// clear rather than about defaults.
		expect(editorStore.dirty).toBe(true);
		expect(editorStore.saveStatus).toBe('saving');

		auth.fireIdentityChange();

		expect(editorStore.dirty).toBe(false);
		expect(editorStore.saveStatus).toBe('idle');
		expect(editorStore.mode).toBe('edit');
		expect(editorStore.lastSaveTime).toBe(0);
		expect(editorStore.externalChange).toBe(false);
	});

	it('is WIDER than resetForDoc, which keeps saveStatus deliberately', async () => {
		// The counterfactual for reusing `resetForDoc` here: it is a DOCUMENT
		// change within one session and keeps `saveStatus`/`lastSaveTime` on
		// purpose. An identity change ends the session, so it needs the wider
		// clear and cannot borrow that one.
		const { editorStore } = await import('./editor.svelte');
		editorStore.setSaveStatus('saved');
		editorStore.setLastSaveTime(99);
		editorStore.resetForDoc();

		expect(editorStore.saveStatus).toBe('saved');
		expect(editorStore.lastSaveTime).toBe(99);
	});
});
