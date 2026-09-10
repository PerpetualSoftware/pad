import { describe, expect, it, vi, beforeEach } from 'vitest';

/**
 * BUG-2991 — store state that BELONGS to the signed-in user was not scoped to
 * them, and nothing dropped it when the user changed.
 *
 * `current`, `workspaces` and an already-published `currentMembership` are not
 * identity-keyed. Logout is an SPA navigation (`authStore.clear()` +
 * `goto('/login')`), so no page load tears them down, and TASK-2988's identity
 * fence only fires when a settle is racing the change — it cannot cover a clean
 * account swap with nothing in flight.
 *
 * Two halves, tested separately because they fail independently:
 *
 *  1. the `authStore.onIdentityChange` subscription drops the previous user's
 *     workspaces, selection and permission state;
 *  2. `create` fences its post-await store writes on the identity that ISSUED
 *     the POST — the append used to run unconditionally, ahead of every fence,
 *     so one user's new workspace landed in the next user's list.
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

// A real minimal `onIdentityChange`, not a stub: the store registers its reset
// through it at module scope, and a stub that swallowed the registration would
// let every assertion here pass against a store with no reset at all.
const auth = vi.hoisted(() => {
	const listeners = new Set<() => void>();
	return {
		userId: 'user-1',
		onIdentityChange(fn: () => void) {
			listeners.add(fn);
			return () => listeners.delete(fn);
		},
		/** Fire what `authStore.clear()` / a sign-in as someone else would fire. */
		fireIdentityChange() {
			for (const fn of listeners) fn();
		},
		/** Drop registrations left by a previous `vi.resetModules()`. */
		resetListeners() {
			listeners.clear();
		},
	};
});
vi.mock('./auth.svelte', () => ({ authStore: auth }));

const OWNER = { role: 'owner', collection_grants: [], item_grants: [] };
const WS = { id: 'w1', slug: 'ws', name: 'WS' };
const OTHER = { id: 'w2', slug: 'other', name: 'Other' };

function deferred<T>() {
	let resolve!: (v: T) => void;
	const promise = new Promise<T>((res) => { resolve = res; });
	return { promise, resolve };
}

describe('workspaceStore: identity change drops the previous user\'s state', () => {
	beforeEach(() => {
		// Fresh module registry per test — the store's state is module state.
		vi.resetModules();
		vi.resetAllMocks();
		// Before the re-import registers again: a leftover listener closes over
		// the PREVIOUS module's state, so firing would reset a store no test is
		// looking at while leaving this one untouched.
		auth.resetListeners();
		auth.userId = 'user-1';
	});

	it('clears workspaces, selection and membership when the signed-in user changes', async () => {
		const { workspaceStore } = await import('./workspace.svelte');
		api.workspaces.get.mockResolvedValue(WS);
		api.workspaces.me.mockResolvedValue(OWNER);
		api.workspaces.list.mockResolvedValue([WS]);

		await workspaceStore.loadAll();
		await workspaceStore.setCurrent('ws');

		// Non-vacuity: there must be something to lose, or the assertions below
		// hold on an empty store for the wrong reason.
		expect(workspaceStore.workspaces).toEqual([WS]);
		expect(workspaceStore.current).toEqual(WS);
		expect(workspaceStore.membershipKnown).toBe(true);
		expect(workspaceStore.isOwner).toBe(true);

		// A clean account change with NOTHING in flight — the case
		// `settleIfCurrent`'s fence cannot reach, because it only runs when a
		// settle arrives to be rejected.
		auth.userId = 'user-2';
		auth.fireIdentityChange();

		expect(workspaceStore.workspaces).toEqual([]);
		expect(workspaceStore.current).toBeNull();
		expect(workspaceStore.currentMembership).toBeNull();
		// Unknown, not "denied": the helpers read unknown as no access, which is
		// the fail-safe direction, and the next resolve supplies a real answer.
		expect(workspaceStore.membershipKnown).toBe(false);
		expect(workspaceStore.isOwner).toBe(false);
	});

	it('does not serve the previous user a replayed answer after the swap', async () => {
		// The answer cache is keyed by user, so user-2 has no entry — but the
		// reset must not have cleared user-1's, or signing back in would refetch
		// what TASK-2988 exists to avoid.
		const { workspaceStore } = await import('./workspace.svelte');
		api.workspaces.get.mockResolvedValue(WS);
		api.workspaces.me.mockResolvedValue(OWNER);

		await workspaceStore.setCurrent('ws');
		expect(workspaceStore.membershipKnown).toBe(true);

		auth.userId = 'user-2';
		auth.fireIdentityChange();
		expect(workspaceStore.membershipKnown).toBe(false);

		// user-1 returns. Their own answer is still cached, so the resolve is
		// answered from it rather than dropping to unknown for the refetch.
		auth.userId = 'user-1';
		auth.fireIdentityChange();
		const held = deferred<typeof OWNER>();
		api.workspaces.me.mockReturnValueOnce(held.promise);
		const back = workspaceStore.setCurrent('ws');
		expect(workspaceStore.membershipKnown).toBe(true);
		expect(workspaceStore.isOwner).toBe(true);
		held.resolve(OWNER);
		await back;
	});

	it('keeps a created workspace out of the next user\'s list', async () => {
		const { workspaceStore } = await import('./workspace.svelte');
		api.workspaces.get.mockResolvedValue(OTHER);

		const created = deferred<typeof OTHER>();
		api.workspaces.create.mockReturnValueOnce(created.promise);
		const pending = workspaceStore.create({ name: 'Other' });

		// The swap lands while the POST is open, exactly as a sign-out plus
		// sign-in during a slow create would.
		auth.userId = 'user-2';
		auth.fireIdentityChange();
		created.resolve(OTHER);
		await pending;

		// The append used to happen BEFORE any fence, so user-1's workspace
		// landed in user-2's list even when the selection was rejected.
		expect(workspaceStore.workspaces).toEqual([]);
		expect(workspaceStore.current).toBeNull();
		expect(workspaceStore.membershipKnown).toBe(false);
		// And no `/me` was issued for it: an answer for a user who is no longer
		// signed in has nowhere to go.
		expect(api.workspaces.me).not.toHaveBeenCalled();
	});

	it('still lists and selects a create that completes under the same user', async () => {
		// The counterfactual for the fence: it must not turn every create into a
		// no-op. Same user throughout, which is the ordinary case.
		const { workspaceStore } = await import('./workspace.svelte');
		api.workspaces.create.mockResolvedValue(OTHER);
		api.workspaces.me.mockResolvedValue(OWNER);

		const ws = await workspaceStore.create({ name: 'Other' });

		expect(ws).toEqual(OTHER);
		expect(workspaceStore.workspaces).toEqual([OTHER]);
		expect(workspaceStore.current).toEqual(OTHER);
		expect(workspaceStore.membershipKnown).toBe(true);
		expect(workspaceStore.isOwner).toBe(true);
	});
});
