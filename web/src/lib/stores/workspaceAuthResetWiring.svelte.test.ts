import { describe, expect, it, vi, beforeEach } from 'vitest';

/**
 * BUG-2991 — the WIRING between the two stores, with neither of them mocked.
 *
 * `workspaceIdentityReset` fires a mocked `onIdentityChange`, and
 * `authIdentityChange` checks when the real one fires. Both can pass while the
 * registration that joins them is gone: the workspace store subscribes at
 * module scope, and nothing in either suite observes that subscription
 * actually happening (CONVE-19 — a direct-call test vouches for the component,
 * not for its binding). Codex round 1 named this gap.
 *
 * So this file mocks ONLY the API client and drives a real `authStore.clear()`.
 */

const api = vi.hoisted(() => ({
	auth: { session: vi.fn() },
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

describe('BUG-2991 wiring: a real sign-out clears the real workspace store', () => {
	beforeEach(() => {
		vi.resetModules();
		vi.resetAllMocks();
	});

	it('drops workspaces, selection and membership on authStore.clear()', async () => {
		const { authStore } = await import('./auth.svelte');
		const { workspaceStore } = await import('./workspace.svelte');

		api.auth.session.mockResolvedValue({
			authenticated: true,
			user: { id: 'u1', email: 'u1@example.com' },
		});
		api.workspaces.list.mockResolvedValue([WS]);
		api.workspaces.get.mockResolvedValue(WS);
		api.workspaces.me.mockResolvedValue(OWNER);

		await authStore.load();
		await workspaceStore.loadAll();
		await workspaceStore.setCurrent('ws');

		// Non-vacuity: there has to be something to lose.
		expect(authStore.userId).toBe('u1');
		expect(workspaceStore.workspaces).toEqual([WS]);
		expect(workspaceStore.current).toEqual(WS);
		expect(workspaceStore.isOwner).toBe(true);

		authStore.clear();

		expect(workspaceStore.workspaces).toEqual([]);
		expect(workspaceStore.current).toBeNull();
		expect(workspaceStore.currentMembership).toBeNull();
		expect(workspaceStore.membershipKnown).toBe(false);
		expect(workspaceStore.isOwner).toBe(false);
	});

	it('drops it on a sign-in as a DIFFERENT user with no sign-out in between', async () => {
		const { authStore } = await import('./auth.svelte');
		const { workspaceStore } = await import('./workspace.svelte');

		api.auth.session.mockResolvedValue({
			authenticated: true,
			user: { id: 'u1', email: 'u1@example.com' },
		});
		api.workspaces.list.mockResolvedValue([WS]);
		api.workspaces.get.mockResolvedValue(WS);
		api.workspaces.me.mockResolvedValue(OWNER);

		await authStore.load();
		await workspaceStore.setCurrent('ws');
		expect(workspaceStore.isOwner).toBe(true);

		api.auth.session.mockResolvedValue({
			authenticated: true,
			user: { id: 'u2', email: 'u2@example.com' },
		});
		await authStore.load();

		expect(workspaceStore.current).toBeNull();
		expect(workspaceStore.membershipKnown).toBe(false);
	});

	it('leaves the store alone when the session refetch returns the SAME user', async () => {
		// The counterfactual, and the one that would bite hardest in production:
		// the root layout refetches the session routinely, and a reset there
		// would blank the sidebar mid-session for no reason.
		const { authStore } = await import('./auth.svelte');
		const { workspaceStore } = await import('./workspace.svelte');

		api.auth.session.mockResolvedValue({
			authenticated: true,
			user: { id: 'u1', email: 'u1@example.com' },
		});
		api.workspaces.list.mockResolvedValue([WS]);
		api.workspaces.get.mockResolvedValue(WS);
		api.workspaces.me.mockResolvedValue(OWNER);

		await authStore.load();
		await workspaceStore.loadAll();
		await workspaceStore.setCurrent('ws');

		await authStore.load();

		expect(workspaceStore.workspaces).toEqual([WS]);
		expect(workspaceStore.current).toEqual(WS);
		expect(workspaceStore.isOwner).toBe(true);
	});

	it('does not clear a workspace list loaded BEFORE the first session answer', async () => {
		// The cold-start ordering the `identityEstablished` guard exists for: the
		// root layout loads the session and the list concurrently, and the
		// session can land second.
		const { authStore } = await import('./auth.svelte');
		const { workspaceStore } = await import('./workspace.svelte');

		api.workspaces.list.mockResolvedValue([WS]);
		await workspaceStore.loadAll();
		expect(workspaceStore.workspaces).toEqual([WS]);

		api.auth.session.mockResolvedValue({
			authenticated: true,
			user: { id: 'u1', email: 'u1@example.com' },
		});
		await authStore.load();

		expect(workspaceStore.workspaces).toEqual([WS]);
	});
});
