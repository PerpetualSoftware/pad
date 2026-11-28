import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/svelte';
import { tick } from 'svelte';
import type { Collection, WorkspaceMembership } from '$lib/types';

// The per-item workspace scope (TASK-4902): owner and edit rights, and the
// collection list, come from the opened item's own workspace, never from the
// shell workspace the global stores describe.
const h = vi.hoisted(() => ({
	me: vi.fn(),
	list: vi.fn(),
	epoch: 0,
	resets: new Set<() => void>(),
	globalMembership: null as WorkspaceMembership | null,
}));
vi.mock('$lib/api/client', () => ({ api: { workspaces: { me: h.me }, collections: { list: h.list } } }));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		identityFence: () => {
			const epoch = h.epoch;
			return () => epoch === h.epoch;
		},
		onIdentityChange: (fn: () => void) => {
			h.resets.add(fn);
			return () => h.resets.delete(fn);
		},
	},
}));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: {
		current: { slug: 'shell', owner_username: 'mara' },
		get currentMembership() { return h.globalMembership; },
		get isOwner() { return h.globalMembership?.role === 'owner'; },
		canEditItem: () => h.globalMembership?.role === 'owner',
		canEditCollection: () => h.globalMembership?.role === 'owner',
	},
}));
vi.mock('$lib/stores/collections.svelte', () => ({
	collectionStore: {
		collections: [{ id: 'g1', slug: 'shell-coll' }],
		collectionsWorkspace: 'pad',
		collectionsAreFreshFor: (ws: string) => ws === 'pad',
		loadCollections: vi.fn(async () => {}),
	},
}));

import { page } from '$app/state';
import { createItemWorkspaceScope } from './workspaceScope.svelte';
import WorkspaceScopeProbe from './__fixtures__/WorkspaceScopeProbe.svelte';

function membership(role: WorkspaceMembership['role']): WorkspaceMembership {
	return {
		role,
		collection_access: 'all',
		visible_collection_ids: [],
		full_access_collection_ids: [],
		collection_grants: [],
		item_grants: [],
	};
}
const coll = (slug: string) => ({ id: `id-${slug}`, slug }) as Collection;
function deferred<T>() {
	let resolve!: (v: T) => void;
	let reject!: (e: unknown) => void;
	const promise = new Promise<T>((res, rej) => {
		resolve = res;
		reject = rej;
	});
	return { promise, resolve, reject };
}
async function settle() {
	for (let i = 0; i < 5; i++) {
		await Promise.resolve();
		await tick();
	}
}
const item = { id: 'i1', collection_id: 'c1' };

beforeEach(() => {
	vi.clearAllMocks();
	h.epoch++;
	h.resets.clear();
	h.globalMembership = membership('owner');
	page.params = {};
});
afterEach(() => cleanup());

describe('createItemWorkspaceScope', () => {
	it('answers from its own /me while the global store says owner', async () => {
		h.me.mockResolvedValue(membership('viewer'));
		h.list.mockResolvedValue([coll('docs')]);
		const scope = createItemWorkspaceScope('kit', 'cloud');
		await settle();
		expect(h.me).toHaveBeenCalledWith('cloud');
		expect(h.list).toHaveBeenCalledWith('cloud');
		expect(scope.slug).toBe('cloud');
		expect(scope.username).toBe('kit');
		expect(scope.currentMembership?.role).toBe('viewer');
		expect(scope.isOwner).toBe(false);
		expect(scope.canEditItem(item)).toBe(false);
		expect(scope.canEditCollection('c1')).toBe(false);
		expect(scope.collections.map((c) => c.slug)).toEqual(['docs']);
		expect(scope.collectionsWorkspace).toBe('cloud');
		expect(scope.collectionsAreFreshFor('cloud')).toBe(true);
		expect(scope.collectionsAreFreshFor('pad')).toBe(false);
		scope.dispose();
	});

	it('an owner answer grants edit', async () => {
		h.me.mockResolvedValue(membership('owner'));
		h.list.mockResolvedValue([]);
		h.globalMembership = membership('viewer');
		const scope = createItemWorkspaceScope('kit', 'cloud');
		await settle();
		expect(scope.isOwner).toBe(true);
		expect(scope.canEditItem(item)).toBe(true);
		scope.dispose();
	});

	it('fails closed before /me answers and after a 403', async () => {
		const me = deferred<WorkspaceMembership>();
		h.me.mockReturnValue(me.promise);
		h.list.mockResolvedValue([coll('docs')]);
		const scope = createItemWorkspaceScope('kit', 'cloud');
		await settle();
		expect(scope.currentMembership).toBeNull();
		expect(scope.isOwner).toBe(false);
		expect(scope.canEditItem(item)).toBe(false);
		expect(scope.canEditCollection('c1')).toBe(false);
		expect(scope.collections).toEqual([]);
		expect(scope.collectionsAreFreshFor('cloud')).toBe(false);
		me.reject(Object.assign(new Error('forbidden'), { status: 403 }));
		await settle();
		expect(scope.currentMembership).toBeNull();
		expect(scope.canEditItem(item)).toBe(false);
		expect(scope.collectionsAreFreshFor('cloud')).toBe(false);
		scope.dispose();
	});

	it('drops a /me that lands after dispose()', async () => {
		const me = deferred<WorkspaceMembership>();
		h.me.mockReturnValue(me.promise);
		h.list.mockResolvedValue([coll('docs')]);
		const scope = createItemWorkspaceScope('kit', 'cloud');
		scope.dispose();
		me.resolve(membership('owner'));
		await settle();
		expect(scope.currentMembership).toBeNull();
		expect(scope.isOwner).toBe(false);
		expect(scope.collections).toEqual([]);
	});

	it('drops a /me that lands after an identity change, and clears an earlier answer', async () => {
		const me = deferred<WorkspaceMembership>();
		h.me.mockReturnValue(me.promise);
		h.list.mockResolvedValue([coll('docs')]);
		const scope = createItemWorkspaceScope('kit', 'cloud');
		h.epoch++;
		for (const r of [...h.resets]) r();
		me.resolve(membership('owner'));
		await settle();
		expect(scope.isOwner).toBe(false);
		expect(scope.collections).toEqual([]);
		scope.dispose();

		h.me.mockResolvedValue(membership('owner'));
		const answered = createItemWorkspaceScope('kit', 'cloud');
		await settle();
		expect(answered.isOwner).toBe(true);
		h.epoch++;
		for (const r of [...h.resets]) r();
		expect(answered.isOwner).toBe(false);
		expect(answered.collectionsAreFreshFor('cloud')).toBe(false);
		answered.dispose();
	});

	it('reloadCollections refetches its own list only', async () => {
		h.me.mockResolvedValue(membership('viewer'));
		h.list.mockResolvedValueOnce([coll('docs')]).mockResolvedValueOnce([coll('docs'), coll('notes')]);
		const scope = createItemWorkspaceScope('kit', 'cloud');
		await settle();
		await scope.reloadCollections('other');
		expect(h.list).toHaveBeenCalledTimes(1);
		await scope.reloadCollections('cloud');
		await settle();
		expect(h.list).toHaveBeenLastCalledWith('cloud');
		expect(scope.collections.map((c) => c.slug)).toEqual(['docs', 'notes']);
		scope.dispose();
	});
});

describe('workspaceScope() with no context', () => {
	it('delegates to the global stores on a workspace route', () => {
		page.params = { username: 'mara', workspace: 'pad' };
		render(WorkspaceScopeProbe);
		const out = screen.getByTestId('scope');
		expect(out.dataset.slug).toBe('pad');
		expect(out.dataset.username).toBe('mara');
		expect(out.dataset.owner).toBe('true');
		expect(out.dataset.role).toBe('owner');
		expect(out.dataset.editItem).toBe('true');
		expect(out.dataset.editCollection).toBe('true');
		expect(out.dataset.collections).toBe('shell-coll');
		expect(out.dataset.fresh).toBe('true');
	});

	it('is closed off a workspace route, whatever the global stores say', () => {
		page.params = {};
		render(WorkspaceScopeProbe);
		const out = screen.getByTestId('scope');
		expect(out.dataset.slug).toBe('');
		expect(out.dataset.owner).toBe('false');
		expect(out.dataset.role).toBe('');
		expect(out.dataset.editItem).toBe('false');
		expect(out.dataset.editCollection).toBe('false');
		expect(out.dataset.collections).toBe('');
		expect(out.dataset.fresh).toBe('false');
	});
});
