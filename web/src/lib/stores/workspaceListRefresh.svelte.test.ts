import { describe, expect, it, vi, beforeEach } from 'vitest';

/**
 * TASK-3279 (PLAN-3002 §1, the stale-list gap): `setCurrent` on a workspace
 * the loaded list does not hold resolves it with a direct GET and used to stop
 * there, so a workspace deep-linked (or joined or restored elsewhere) never
 * appeared in the list. It now refreshes the list, once per slug per page load.
 */

const api = vi.hoisted(() => ({
	workspaces: {
		get: vi.fn(),
		me: vi.fn(),
		list: vi.fn(),
		create: vi.fn(),
	},
}));

vi.mock('$lib/api/client', () => ({ api }));

// The answer cache is keyed by signed-in user as well as slug, so the identity
// has to be controllable to test that a second user inherits nothing.
//
// `onIdentityChange` is a REAL minimal implementation rather than a no-op stub
// (BUG-2991): the store registers a reset through it at module scope, and a
// stub that swallowed the registration would make every test here pass against
// a store whose identity reset had been deleted.
const auth = vi.hoisted(() => {
	const listeners = new Set<() => void>();
	return {
		userId: 'user-1',
		onIdentityChange(fn: () => void) {
			listeners.add(fn);
			return () => listeners.delete(fn);
		},
		/** Test-only: fire what `authStore.clear()` / a sign-in would fire. */
		fireIdentityChange() {
			for (const fn of listeners) fn();
		},
		/** Test-only: drop registrations from a previous `vi.resetModules()`. */
		resetListeners() {
			listeners.clear();
		},
	};
});
vi.mock('./auth.svelte', () => ({ authStore: auth }));

const OWNER = { role: 'owner', collection_grants: [], item_grants: [] };
const HOME = { id: 'w1', slug: 'home', name: 'Home' };
const DEEP = { id: 'w2', slug: 'deep', name: 'Deep' };

async function settle() {
	for (let i = 0; i < 10; i++) await Promise.resolve();
}

describe('workspaceStore: a setCurrent miss refreshes the list', () => {
	beforeEach(() => {
		vi.resetModules();
		vi.resetAllMocks();
		auth.resetListeners();
		auth.userId = 'user-1';
		api.workspaces.me.mockResolvedValue(OWNER);
	});

	it('refreshes the list once when a resolvable workspace is missing from it', async () => {
		const { workspaceStore } = await import('./workspace.svelte');
		api.workspaces.list.mockResolvedValueOnce([HOME]);
		await workspaceStore.loadAll();
		expect(workspaceStore.workspaces.map((w) => w.slug)).toEqual(['home']);

		api.workspaces.get.mockResolvedValue(DEEP);
		api.workspaces.list.mockResolvedValue([HOME, DEEP]);
		await workspaceStore.setCurrent('deep');
		await settle();

		expect(api.workspaces.list).toHaveBeenCalledTimes(2);
		expect(workspaceStore.workspaces.map((w) => w.slug)).toEqual(['home', 'deep']);
	});

	it('does not refresh for a workspace the list already holds, or one that does not resolve', async () => {
		const { workspaceStore } = await import('./workspace.svelte');
		api.workspaces.list.mockResolvedValueOnce([HOME]);
		await workspaceStore.loadAll();

		await workspaceStore.setCurrent('home');
		api.workspaces.get.mockRejectedValueOnce(new Error('not found'));
		await workspaceStore.setCurrent('hidden');
		await settle();

		expect(api.workspaces.list).toHaveBeenCalledTimes(1);
	});

	it('refreshes at most once per slug, even when the list never carries it', async () => {
		const { workspaceStore } = await import('./workspace.svelte');
		api.workspaces.list.mockResolvedValue([HOME]);
		await workspaceStore.loadAll();
		api.workspaces.get.mockResolvedValue(DEEP);

		await workspaceStore.setCurrent('deep');
		await settle();
		await workspaceStore.setCurrent('deep');
		await settle();

		expect(api.workspaces.list).toHaveBeenCalledTimes(2);
	});

	it('does not refresh before a list has committed (the first one is already in flight)', async () => {
		const { workspaceStore } = await import('./workspace.svelte');
		api.workspaces.get.mockResolvedValue(DEEP);
		api.workspaces.list.mockResolvedValue([HOME, DEEP]);

		await workspaceStore.setCurrent('deep');
		await settle();

		expect(api.workspaces.list).not.toHaveBeenCalled();
	});

	it('tries again on a later landing when the refresh failed', async () => {
		const { workspaceStore } = await import('./workspace.svelte');
		api.workspaces.list.mockResolvedValueOnce([HOME]);
		await workspaceStore.loadAll();
		api.workspaces.get.mockResolvedValue(DEEP);

		api.workspaces.list.mockRejectedValueOnce(new Error('network down'));
		await workspaceStore.setCurrent('deep');
		await settle();
		api.workspaces.list.mockResolvedValueOnce([HOME, DEEP]);
		await workspaceStore.setCurrent('deep');
		await settle();

		expect(api.workspaces.list).toHaveBeenCalledTimes(3);
		expect(workspaceStore.workspaces.map((w) => w.slug)).toEqual(['home', 'deep']);
	});
});
