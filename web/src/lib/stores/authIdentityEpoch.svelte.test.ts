import { describe, expect, it, vi, beforeEach } from 'vitest';

/**
 * BUG-3005 — `authStore.identityEpoch` / `identityFence()`.
 *
 * The epoch exists so a store that issues a request as one user can refuse the
 * response if it settles as another. Its whole claim over the user-id
 * comparison already in the tree is that it is ORDERED: A → B → A must not
 * look like "no change".
 */

const session = vi.hoisted(() => ({ value: null as unknown }));

vi.mock('$lib/api/client', () => ({
	api: { auth: { session: vi.fn(async () => session.value) } },
}));

function user(id: string) {
	return { authenticated: true, user: { id, email: `${id}@example.com` } };
}

describe('authStore identity epoch', () => {
	beforeEach(() => {
		vi.resetModules();
		session.value = null;
	});

	it('does not move on the FIRST resolution of the session', async () => {
		// Same reasoning as onIdentityChange's silence on the baseline: a cold
		// start has nothing stale, and a fence captured before an identity was
		// established must not start refusing once one appears.
		const { authStore } = await import('./auth.svelte');
		const isSameIdentity = authStore.identityFence();

		session.value = user('u1');
		await authStore.load();

		expect(authStore.identityEpoch).toBe(0);
		expect(isSameIdentity()).toBe(true);
	});

	it('moves on sign-out, and a fence taken before it refuses', async () => {
		const { authStore } = await import('./auth.svelte');
		session.value = user('u1');
		await authStore.load();

		const isSameIdentity = authStore.identityFence();
		// PRECONDITION: current before the change, or "it refused" is a claim
		// about a fence that never accepted anything.
		expect(isSameIdentity()).toBe(true);

		authStore.clear();

		expect(authStore.identityEpoch).toBe(1);
		expect(isSameIdentity()).toBe(false);
	});

	it('REFUSES a straggler from the first A session after A -> B -> A', async () => {
		// The reason this is an ordinal and not the user id. Every id comparison
		// in this scenario reports "same user", because it IS the same user —
		// and the session that issued the request is gone.
		const { authStore } = await import('./auth.svelte');
		session.value = user('a');
		await authStore.load();

		// A request issued in A's first session captures its fence here.
		const issuedInFirstA = authStore.identityFence();
		const userIdAtIssue = authStore.userId;

		authStore.clear();
		session.value = user('b');
		await authStore.load();
		authStore.clear();
		session.value = user('a');
		await authStore.load();

		// The comparison the tree already had says CURRENT — this is the hole.
		expect(authStore.userId).toBe(userIdAtIssue);
		// The epoch says otherwise, which is the fix.
		expect(issuedInFirstA()).toBe(false);
		expect(authStore.identityEpoch).toBe(4);
	});

	it('stays put when a session refetch returns the SAME user', async () => {
		// The counterfactual for every test above: a fence that refuses on a
		// no-op refetch would drop live data on the root layout's ordinary
		// session poll, which is a worse defect than the one being fixed.
		const { authStore } = await import('./auth.svelte');
		session.value = user('u1');
		await authStore.load();

		const isSameIdentity = authStore.identityFence();
		const before = authStore.identityEpoch;
		await authStore.load();

		expect(authStore.identityEpoch).toBe(before);
		expect(isSameIdentity()).toBe(true);
	});

	it('is already bumped when identity listeners run', async () => {
		// A listener's expected shape is "drop state, reload it". The reload
		// captures a fence WHILE the listener runs, so a bump ordered after the
		// listeners would hand it the old epoch and refuse its own response.
		const { authStore } = await import('./auth.svelte');
		session.value = user('u1');
		await authStore.load();

		let fenceTakenInsideListener: (() => boolean) | null = null;
		authStore.onIdentityChange(() => {
			fenceTakenInsideListener = authStore.identityFence();
		});
		authStore.clear();

		expect(fenceTakenInsideListener).not.toBeNull();
		expect(fenceTakenInsideListener!()).toBe(true);
	});
});
