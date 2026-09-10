import { describe, expect, it, vi, beforeEach } from 'vitest';

/**
 * TASK-2988 — a REPEAT resolution of a workspace this session has already
 * answered must not drop `membershipKnown` to false.
 *
 * Consumers cannot tell "not fetched yet" from "no access" (that is the whole
 * reason BUG-2978 added the flag), so a false window makes every
 * permission-gated `{#if}` block unmount for the length of a refetch — taking
 * the in-progress form state of any dialog inside one with it. The repeat is
 * routine rather than exotic: the workspace layout calls `recoverIfMissing`
 * from its sync callback on every sync result, and that calls `setCurrent`
 * whenever `current` is null or names a different workspace.
 *
 * The counterfactual legs matter as much as the regression ones: the entry
 * clear exists so helpers can never answer with ANOTHER workspace's grants,
 * and this fix must not weaken that into "membership is always sticky".
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
const VIEWER = { role: 'viewer', collection_grants: [], item_grants: [] };
const WS = { id: 'w1', slug: 'ws', name: 'WS' };
const OTHER = { id: 'w2', slug: 'other', name: 'Other' };

/** A promise plus its resolver, so a test can hold `/me` open and observe the window. */
function deferred<T>() {
	let resolve!: (v: T) => void;
	let reject!: (e: unknown) => void;
	const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
	return { promise, resolve, reject };
}

describe('workspaceStore: repeat resolution of an already-answered workspace', () => {
	beforeEach(() => {
		// Fresh module registry per test: the answer cache is module state, and a
		// test that inherited a previous test's cache would pass for the wrong
		// reason — which is exactly the failure this suite is built to catch.
		vi.resetModules();
		vi.resetAllMocks();
		// Before the module re-registers: each fresh import of the store adds a
		// listener, and leftovers from previous tests close over THAT module's
		// state, so firing would reset a store no test is looking at.
		auth.resetListeners();
		auth.userId = 'user-1';
	});

	it('keeps membershipKnown and isOwner true across a second setCurrent for the same slug', async () => {
		const { workspaceStore } = await import('./workspace.svelte');
		api.workspaces.get.mockResolvedValue(WS);

		api.workspaces.me.mockResolvedValueOnce(OWNER);
		await workspaceStore.setCurrent('ws');
		expect(workspaceStore.membershipKnown).toBe(true);
		expect(workspaceStore.isOwner).toBe(true);

		// Second resolution, `/me` held open — the window a refetch occupies.
		const second = deferred<typeof OWNER>();
		api.workspaces.me.mockReturnValueOnce(second.promise);
		const pending = workspaceStore.setCurrent('ws');

		expect(workspaceStore.membershipKnown).toBe(true);
		expect(workspaceStore.isOwner).toBe(true);
		expect(workspaceStore.currentMembership).toEqual(OWNER);

		second.resolve(OWNER);
		await pending;
		expect(workspaceStore.membershipKnown).toBe(true);
		expect(workspaceStore.isOwner).toBe(true);
	});

	it('keeps them true through the recoverIfMissing sequence that reaches this in the app', async () => {
		const { workspaceStore } = await import('./workspace.svelte');
		api.workspaces.get.mockResolvedValue(WS);
		api.workspaces.list.mockResolvedValue([WS, OTHER]);

		// On `ws`, membership answered: this is the tab the dialog is open in.
		api.workspaces.me.mockResolvedValueOnce(OWNER);
		await workspaceStore.setCurrent('ws');
		expect(workspaceStore.isOwner).toBe(true);

		// A create points `current` at a DIFFERENT workspace while that route
		// stays mounted — the step that makes `current.slug !== ws` true.
		api.workspaces.create.mockResolvedValue(OTHER);
		api.workspaces.me.mockResolvedValueOnce(OWNER);
		await workspaceStore.create({ name: 'Other' });
		expect(workspaceStore.current?.slug).toBe('other');

		// The mounted route's next sync result. `recoverIfMissing` sees the
		// mismatch and re-resolves `ws`; `/me` is held open so the refetch
		// window is observable.
		const refetch = deferred<typeof OWNER>();
		api.workspaces.me.mockReturnValueOnce(refetch.promise);
		const pending = workspaceStore.recoverIfMissing('ws');

		// Unfixed, membershipKnown is false here, and every permission-gated
		// block that reads the store DIRECTLY unmounts. Consumers holding a
		// sticky copy (the settings page, the dashboard CTA) ride it out — which
		// is why the four sites this was found at are the ones that did not.
		expect(workspaceStore.membershipKnown).toBe(true);
		expect(workspaceStore.isOwner).toBe(true);

		refetch.resolve(OWNER);
		await pending;
		expect(workspaceStore.membershipKnown).toBe(true);
		expect(workspaceStore.isOwner).toBe(true);
	});

	it('serves a cached DENIAL on repeat rather than flickering to unknown', async () => {
		const { workspaceStore } = await import('./workspace.svelte');
		api.workspaces.get.mockResolvedValue(WS);

		api.workspaces.me.mockRejectedValueOnce(new Error('403'));
		await workspaceStore.setCurrent('ws');
		expect(workspaceStore.membershipKnown).toBe(true);
		expect(workspaceStore.currentMembership).toBeNull();

		const second = deferred<unknown>();
		api.workspaces.me.mockReturnValueOnce(second.promise);
		const pending = workspaceStore.setCurrent('ws');

		// Denied is an answer: it stays the answer through the refetch, and the
		// consumer never sees the "wait" state it cannot distinguish from it.
		expect(workspaceStore.membershipKnown).toBe(true);
		expect(workspaceStore.currentMembership).toBeNull();
		expect(workspaceStore.isOwner).toBe(false);

		second.reject(new Error('403'));
		await pending;
		expect(workspaceStore.membershipKnown).toBe(true);
	});

	it('still clears for a workspace this session has NOT answered', async () => {
		const { workspaceStore } = await import('./workspace.svelte');
		api.workspaces.get.mockImplementation(async (slug: string) =>
			slug === 'ws' ? WS : OTHER
		);

		api.workspaces.me.mockResolvedValueOnce(OWNER);
		await workspaceStore.setCurrent('ws');
		expect(workspaceStore.isOwner).toBe(true);

		// Switching to a workspace with no answer yet: the entry clear must
		// still happen, or `ws`'s grants would answer for `other`.
		const first = deferred<typeof VIEWER>();
		api.workspaces.me.mockReturnValueOnce(first.promise);
		const pending = workspaceStore.setCurrent('other');

		expect(workspaceStore.membershipKnown).toBe(false);
		expect(workspaceStore.currentMembership).toBeNull();
		expect(workspaceStore.isOwner).toBe(false);

		first.resolve(VIEWER);
		await pending;
		expect(workspaceStore.membershipKnown).toBe(true);
		expect(workspaceStore.currentRole).toBe('viewer');
	});

	it('never serves one workspace\'s answer for another, even with both answered', async () => {
		const { workspaceStore } = await import('./workspace.svelte');
		api.workspaces.get.mockImplementation(async (slug: string) =>
			slug === 'ws' ? WS : OTHER
		);

		api.workspaces.me.mockResolvedValueOnce(OWNER);
		await workspaceStore.setCurrent('ws');
		api.workspaces.me.mockResolvedValueOnce(VIEWER);
		await workspaceStore.setCurrent('other');
		expect(workspaceStore.currentRole).toBe('viewer');

		// Back to `ws`, refetch held open. The served answer must be `ws`'s
		// owner, not the `other` viewer that was current a moment ago — this is
		// the leg that would catch a "sticky whatever was last" regression.
		const back = deferred<typeof OWNER>();
		api.workspaces.me.mockReturnValueOnce(back.promise);
		const pending = workspaceStore.setCurrent('ws');
		expect(workspaceStore.currentRole).toBe('owner');

		back.resolve(OWNER);
		await pending;
		expect(workspaceStore.currentRole).toBe('owner');
	});

	it('a later answer replaces the cached one, so a role change is not held forever', async () => {
		const { workspaceStore } = await import('./workspace.svelte');
		api.workspaces.get.mockResolvedValue(WS);

		api.workspaces.me.mockResolvedValueOnce(OWNER);
		await workspaceStore.setCurrent('ws');
		expect(workspaceStore.isOwner).toBe(true);

		// Demoted server-side. The serve is only for the refetch window; the
		// refetch itself is what the consumer ends up with.
		api.workspaces.me.mockResolvedValueOnce(VIEWER);
		await workspaceStore.setCurrent('ws');
		expect(workspaceStore.isOwner).toBe(false);
		expect(workspaceStore.currentRole).toBe('viewer');

		// And the NEW answer is what a subsequent repeat serves.
		const third = deferred<typeof VIEWER>();
		api.workspaces.me.mockReturnValueOnce(third.promise);
		const pending = workspaceStore.setCurrent('ws');
		expect(workspaceStore.currentRole).toBe('viewer');
		third.resolve(VIEWER);
		await pending;
	});

	it('does not serve one user\'s answer to the NEXT user after a logout', async () => {
		// Logout is an SPA navigation (`authStore.clear()` + `goto('/login')`), so
		// this module — and the cache in it — outlives a sign-out. Keyed by slug
		// alone, the next user to open the same workspace would be handed the
		// previous user's grants until their own /me settled. Found by codex
		// round 3 on the first version of this fix, which was keyed by slug.
		const { workspaceStore } = await import('./workspace.svelte');
		api.workspaces.get.mockResolvedValue(WS);

		api.workspaces.me.mockResolvedValueOnce(OWNER);
		await workspaceStore.setCurrent('ws');
		expect(workspaceStore.isOwner).toBe(true);

		// Second user, same workspace slug, /me held open.
		auth.userId = 'user-2';
		const theirs = deferred<typeof VIEWER>();
		api.workspaces.me.mockReturnValueOnce(theirs.promise);
		const pending = workspaceStore.setCurrent('ws');

		expect(workspaceStore.membershipKnown).toBe(false);
		expect(workspaceStore.currentMembership).toBeNull();
		expect(workspaceStore.isOwner).toBe(false);

		theirs.resolve(VIEWER);
		await pending;
		expect(workspaceStore.currentRole).toBe('viewer');
	});

	it('discards a /me that settles after the user changed mid-flight', async () => {
		// The identity fence, which the cache KEY alone does not provide: keying
		// stops a later user reading an earlier answer, but a request already in
		// flight when the user changes would still publish its answer and record
		// it under the NEW user's key. Two fences, two races — a sign-in does not
		// advance membershipSeq (codex round 4).
		const { workspaceStore } = await import('./workspace.svelte');
		api.workspaces.get.mockResolvedValue(WS);

		const theirs = deferred<typeof OWNER>();
		api.workspaces.me.mockReturnValueOnce(theirs.promise);
		const pending = workspaceStore.setCurrent('ws');

		// Sign-in as someone else lands while user-1's /me is still open.
		auth.userId = 'user-2';
		theirs.resolve(OWNER);
		await pending;

		// user-1's owner answer must neither be published...
		expect(workspaceStore.isOwner).toBe(false);
		expect(workspaceStore.membershipKnown).toBe(false);

		// ...nor be sitting in the cache under user-2, where user-2's own next
		// resolution would be served it.
		const mine = deferred<typeof VIEWER>();
		api.workspaces.me.mockReturnValueOnce(mine.promise);
		const second = workspaceStore.setCurrent('ws');
		expect(workspaceStore.membershipKnown).toBe(false);
		expect(workspaceStore.isOwner).toBe(false);

		mine.resolve(VIEWER);
		await second;
		expect(workspaceStore.currentRole).toBe('viewer');
	});

	it('clears a replayed answer when the user changes before the refetch settles', async () => {
		// The serve at entry is correct at the instant it happens, so the fence
		// on the SETTLE is what has to notice the change — and noticing is not
		// enough: dropping the late answer silently would leave the replayed
		// owner read on screen for the previous user. An identity mismatch
		// therefore clears to unknown rather than returning (codex round 5).
		const { workspaceStore } = await import('./workspace.svelte');
		api.workspaces.get.mockResolvedValue(WS);

		api.workspaces.me.mockResolvedValueOnce(OWNER);
		await workspaceStore.setCurrent('ws');
		expect(workspaceStore.isOwner).toBe(true);

		// Repeat resolve replays the cached owner answer, then the account
		// changes while the refetch is still open.
		const refetch = deferred<typeof OWNER>();
		api.workspaces.me.mockReturnValueOnce(refetch.promise);
		const pending = workspaceStore.setCurrent('ws');
		expect(workspaceStore.isOwner).toBe(true);

		auth.userId = 'user-2';
		refetch.resolve(OWNER);
		await pending;

		expect(workspaceStore.isOwner).toBe(false);
		expect(workspaceStore.membershipKnown).toBe(false);
		expect(workspaceStore.currentMembership).toBeNull();

		// And `current` is dropped, which is what makes the clear self-healing:
		// `recoverIfMissing` re-resolves on a null `current` and runs on every
		// sync result, so the new user gets a real answer without needing a
		// navigation. Left in place, they would sit unknown indefinitely.
		expect(workspaceStore.current).toBeNull();

		api.workspaces.me.mockResolvedValueOnce(VIEWER);
		api.workspaces.list.mockResolvedValue([WS]);
		await workspaceStore.recoverIfMissing('ws');
		expect(workspaceStore.membershipKnown).toBe(true);
		expect(workspaceStore.currentRole).toBe('viewer');
	});

	it('does not cache a create issued by one user under the next user', async () => {
		// `create` captures identity BEFORE its POST. Capturing after would key
		// the first user's membership by the SECOND user's id — the exact leak
		// the key exists to close, reintroduced through the other door (codex
		// round 5).
		const { workspaceStore } = await import('./workspace.svelte');
		api.workspaces.get.mockResolvedValue(OTHER);

		const created = deferred<typeof OTHER>();
		api.workspaces.create.mockReturnValueOnce(created.promise);
		// Staged and expected to go UNCONSUMED. Before BUG-2991 the create ran
		// on past its POST and issued this `/me`, and the only thing standing
		// between user-1's answer and user-2's store was `settleIfCurrent`
		// rejecting it at the very end. The fence now returns before the fetch:
		// a `/me` for a user who is no longer signed in has no answer worth
		// having, and every write that used to happen ahead of that rejection —
		// the list append, the selection — no longer happens either.
		api.workspaces.me.mockResolvedValueOnce(OWNER);
		const pending = workspaceStore.create({ name: 'Other' });

		// Sign-in as someone else lands while the POST is open.
		auth.userId = 'user-2';
		created.resolve(OTHER);
		await pending;

		// The create is fenced at the identity check, so no membership was even
		// requested for it.
		expect(api.workspaces.me).not.toHaveBeenCalled();
		// user-1's owner answer must not be published for user-2...
		expect(workspaceStore.isOwner).toBe(false);
		expect(workspaceStore.membershipKnown).toBe(false);
		// ...and user-1's workspace must not be in user-2's list or selected
		// (BUG-2991: the append used to run unconditionally, ahead of every
		// fence, so it landed even when the selection was rejected).
		expect(workspaceStore.workspaces).toEqual([]);
		expect(workspaceStore.current).toBeNull();

		// ...nor be waiting in user-2's cache for their own next resolve.
		// The staged OWNER above is dropped rather than left to be picked up by
		// the `setCurrent` below, which would answer this leg for the wrong
		// reason.
		api.workspaces.me.mockReset();
		const mine = deferred<typeof VIEWER>();
		api.workspaces.me.mockReturnValueOnce(mine.promise);
		const second = workspaceStore.setCurrent('other');
		expect(workspaceStore.membershipKnown).toBe(false);
		expect(workspaceStore.isOwner).toBe(false);

		mine.resolve(VIEWER);
		await second;
		expect(workspaceStore.currentRole).toBe('viewer');
	});
});
