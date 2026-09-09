import { describe, expect, it, vi, beforeEach } from 'vitest';

/**
 * BUG-2978 — `membershipKnown` says whether a null `currentMembership` is an
 * ANSWER ("no access") or merely NOT YET FETCHED. Consumers cache permissions
 * through the false window, so a path that leaves it false forever is a cache
 * that never updates again.
 *
 * The create-failure leg is the one codex round 2 found: `create()` clears
 * membership at entry, and a rejected create returned before anything settled
 * the flag.
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

const OWNER = { role: 'owner', collection_grants: [], item_grants: [] };
const WS = { id: 'w1', slug: 'ws', name: 'WS' };

describe('workspaceStore.membershipKnown', () => {
	beforeEach(() => {
		vi.resetAllMocks();
	});

	it('is false while a membership fetch is in flight and true once it settles', async () => {
		const { workspaceStore } = await import('./workspace.svelte');
		let release: (v: unknown) => void = () => {};
		api.workspaces.get.mockResolvedValue(WS);
		api.workspaces.me.mockReturnValue(new Promise((r) => { release = r; }));

		const pending = workspaceStore.setCurrent('ws');
		expect(workspaceStore.membershipKnown).toBe(false);

		release(OWNER);
		await pending;
		expect(workspaceStore.membershipKnown).toBe(true);
		expect(workspaceStore.isOwner).toBe(true);
	});

	it('is true after a 403 — a denial is an answer, not a pending state', async () => {
		const { workspaceStore } = await import('./workspace.svelte');
		api.workspaces.get.mockResolvedValue(WS);
		api.workspaces.me.mockRejectedValue(new Error('403'));

		await workspaceStore.setCurrent('ws');
		expect(workspaceStore.membershipKnown).toBe(true);
		expect(workspaceStore.currentMembership).toBeNull();
		expect(workspaceStore.isOwner).toBe(false);
	});

	it('is true when the workspace itself does not resolve', async () => {
		const { workspaceStore } = await import('./workspace.svelte');
		api.workspaces.get.mockRejectedValue(new Error('404'));

		await workspaceStore.setCurrent('missing');
		expect(workspaceStore.membershipKnown).toBe(true);
	});

	it('is false while the WORKSPACE is still resolving, not just the /me', async () => {
		// The contract is the whole replacing call, so the window has to open
		// before `/me` is even reached (codex round 3).
		const { workspaceStore } = await import('./workspace.svelte');
		let releaseGet: (v: unknown) => void = () => {};
		api.workspaces.get.mockReturnValue(new Promise((r) => { releaseGet = r; }));
		api.workspaces.me.mockResolvedValue(OWNER);

		const pending = workspaceStore.setCurrent('ws');
		expect(workspaceStore.membershipKnown).toBe(false);

		releaseGet(WS);
		await pending;
		expect(workspaceStore.membershipKnown).toBe(true);
	});

	it('leaves the CURRENT workspace untouched when a create fails', async () => {
		// A failed create says nothing about the workspace you are still looking
		// at. The first version of this fix cleared membership at entry and then
		// settled the flag on the failure path, which told every consumer the
		// current workspace was now a definitive "no access" — hiding a mounted
		// settings page's owner controls until the next setCurrent (codex round
		// 3). The flag alone cannot catch that, so this asserts the membership.
		const { workspaceStore } = await import('./workspace.svelte');
		api.workspaces.get.mockResolvedValue(WS);
		api.workspaces.me.mockResolvedValue(OWNER);
		await workspaceStore.setCurrent('ws');
		expect(workspaceStore.isOwner).toBe(true);

		api.workspaces.create.mockRejectedValue(new Error('plan limit'));
		await expect(workspaceStore.create({ name: 'nope' })).rejects.toThrow('plan limit');

		expect(workspaceStore.membershipKnown).toBe(true);
		expect(workspaceStore.currentMembership).not.toBeNull();
		expect(workspaceStore.isOwner).toBe(true);
	});
});
