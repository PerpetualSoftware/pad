import { describe, expect, it, vi, beforeEach } from 'vitest';

/**
 * BUG-2991 — `authStore.onIdentityChange`, the notification the workspace store
 * hangs its identity reset on.
 *
 * The subscription exists so no sign-out site has to remember to clear
 * user-scoped state. That only works if it fires on every real change of user
 * AND stays silent otherwise, because its listeners are destructive: a
 * spurious fire wipes a live store.
 */

const session = vi.hoisted(() => ({ value: null as unknown }));

vi.mock('$lib/api/client', () => ({
	api: { auth: { session: vi.fn(async () => session.value) } },
}));

function user(id: string) {
	return { authenticated: true, user: { id, email: `${id}@example.com` } };
}

describe('authStore.onIdentityChange', () => {
	beforeEach(() => {
		vi.resetModules();
		session.value = null;
	});

	it('stays silent on the FIRST resolution of the session', async () => {
		// The root layout loads the session and the workspace list concurrently.
		// A cold start has nothing stale to drop, and firing here would clear a
		// `workspaces` array that had just been populated by the other request —
		// so the first answer only records the baseline.
		const { authStore } = await import('./auth.svelte');
		const fired = vi.fn();
		authStore.onIdentityChange(fired);

		session.value = user('u1');
		await authStore.load();

		expect(authStore.userId).toBe('u1');
		expect(fired).not.toHaveBeenCalled();
	});

	it('fires on sign-OUT after an established identity', async () => {
		const { authStore } = await import('./auth.svelte');
		session.value = user('u1');
		await authStore.load();

		const fired = vi.fn();
		authStore.onIdentityChange(fired);
		authStore.clear();

		expect(fired).toHaveBeenCalledTimes(1);
		expect(authStore.userId).toBe('');
	});

	it('fires on a swap to a different user with no sign-out in between', async () => {
		const { authStore } = await import('./auth.svelte');
		session.value = user('u1');
		await authStore.load();

		const fired = vi.fn();
		authStore.onIdentityChange(fired);

		session.value = user('u2');
		await authStore.load();

		expect(fired).toHaveBeenCalledTimes(1);
		expect(authStore.userId).toBe('u2');
	});

	it('stays silent when a refetch returns the SAME user', async () => {
		// The common case: the layout refetches the session and nothing changed.
		// Firing here would clear the store mid-session for no reason.
		const { authStore } = await import('./auth.svelte');
		session.value = user('u1');
		await authStore.load();

		const fired = vi.fn();
		authStore.onIdentityChange(fired);

		session.value = user('u1');
		await authStore.load();

		expect(fired).not.toHaveBeenCalled();
	});

	it('fires once, not twice, for a sign-out followed by a sign-in as the same user', async () => {
		const { authStore } = await import('./auth.svelte');
		session.value = user('u1');
		await authStore.load();

		const fired = vi.fn();
		authStore.onIdentityChange(fired);

		authStore.clear();
		expect(fired).toHaveBeenCalledTimes(1);

		session.value = user('u1');
		await authStore.load();
		// Back to u1 from '' is a real transition and must fire: the store was
		// cleared in between, so it has to be re-resolved.
		expect(fired).toHaveBeenCalledTimes(2);
	});

	it('does NOT fire when the session request FAILS', async () => {
		// BUG-2991, codex round 9. A rejection is a fetch error, not an identity
		// signal — "not authenticated" comes back as a RESOLVED session, which
		// is what `load`'s re-throw exists to distinguish. Treating a failed
		// poll as a sign-out was destructive: listeners drop the workspace
		// store, re-arm the layout's load latch and close the create-workspace
		// modal, so a user who is still signed in lost an in-flight create or
		// import with no toast, no navigation and no close. `/console/billing`
		// polls `load()`, so a single flaky request reached it.
		const { authStore } = await import('./auth.svelte');
		const api = (await import('$lib/api/client')).api as unknown as {
			auth: { session: ReturnType<typeof vi.fn> };
		};
		session.value = user('u1');
		await authStore.load();

		const fired = vi.fn();
		authStore.onIdentityChange(fired);

		api.auth.session.mockRejectedValueOnce(new Error('network'));
		await expect(authStore.load()).rejects.toThrow('network');

		expect(fired).not.toHaveBeenCalled();
	});

	it('still fires on a real sign-out that follows a failed request', async () => {
		// The counterfactual: silencing the failure path must not silence the
		// real one that comes after it.
		const { authStore } = await import('./auth.svelte');
		const api = (await import('$lib/api/client')).api as unknown as {
			auth: { session: ReturnType<typeof vi.fn> };
		};
		session.value = user('u1');
		await authStore.load();

		const fired = vi.fn();
		authStore.onIdentityChange(fired);

		api.auth.session.mockRejectedValueOnce(new Error('network'));
		await expect(authStore.load()).rejects.toThrow('network');
		expect(fired).not.toHaveBeenCalled();

		authStore.clear();
		expect(fired).toHaveBeenCalledTimes(1);
	});

	it('fires on a RESOLVED unauthenticated session, which is what a real sign-out looks like', async () => {
		// The other half of the rejection rule, and the one that keeps it
		// honest (codex round 10). `/auth/session` is public and answers 200
		// with `authenticated: false` for a missing or expired session, so an
		// ordinary sign-out arrives RESOLVED. If silencing the rejection path
		// had also silenced this, the whole identity reset would have stopped
		// working for the commonest case there is.
		const { authStore } = await import('./auth.svelte');
		session.value = user('u1');
		await authStore.load();

		const fired = vi.fn();
		authStore.onIdentityChange(fired);

		session.value = { authenticated: false, user: null };
		await authStore.load();

		expect(authStore.userId).toBe('');
		expect(fired).toHaveBeenCalledTimes(1);
	});

	it('unsubscribes', async () => {
		const { authStore } = await import('./auth.svelte');
		session.value = user('u1');
		await authStore.load();

		const fired = vi.fn();
		const off = authStore.onIdentityChange(fired);
		off();
		authStore.clear();

		expect(fired).not.toHaveBeenCalled();
	});
});
