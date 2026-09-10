// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// BUG-2991, codex round 1 — the root layout issues `workspaceStore.loadAll()`
// once, behind a one-shot `workspacesRequested` latch.
//
// The store now DROPS the previous user's workspace list when the signed-in
// user changes. Logout is an SPA navigation, so this layout stays mounted and
// the latch stays true — which means the fix, on its own, would leave the NEW
// user with an empty list and nothing to refill it. That is TASK-2200's symptom
// exactly: a sidebar with no links, and only F5 to fix it.
//
// So the latch is re-armed on an identity change, and this file is the only
// place that binding is observable (CONVE-19).
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { createRawSnippet, tick } from 'svelte';

const api = vi.hoisted(() => ({
	auth: { session: vi.fn() },
	workspaces: { list: vi.fn(), get: vi.fn(), me: vi.fn(), create: vi.fn() },
}));

vi.mock('$lib/api/client', () => ({
	api,
	setAccessRevokedHandler: () => {},
	setRateLimitHandler: () => {},
	isPlanLimitError: () => false,
	planLimitMessage: () => '',
}));

// `goto` must not run: the unauthenticated branches call it, and jsdom has no
// navigation. Nothing here asserts on it.
const identityReload = vi.hoisted(() => ({ reloadForIdentityChange: vi.fn() }));
vi.mock('$lib/stores/identityReload.svelte', () => identityReload);

vi.mock('$app/navigation', () => ({
	goto: vi.fn(),
	beforeNavigate: () => {},
	afterNavigate: () => {},
	invalidateAll: vi.fn(),
	pushState: vi.fn(),
	replaceState: vi.fn(),
}));

import Layout from './+layout.svelte';
import { authStore } from '$lib/stores/auth.svelte';
import { workspaceStore } from '$lib/stores/workspace.svelte';
import { page } from '$app/state';

const childSnippet = createRawSnippet(() => ({ render: () => `<div>child</div>` }));

const U1_WS = { id: 'w1', slug: 'ws', name: 'WS' };
const U2_WS = { id: 'w2', slug: 'other', name: 'Other' };

function sessionFor(id: string) {
	return { authenticated: true, user: { id, email: `${id}@example.com` } };
}

/** Let onMount's await chain, the effect and the store's promises land. */
async function settle(): Promise<void> {
	for (let i = 0; i < 10; i++) {
		await Promise.resolve();
		await tick();
	}
}

beforeEach(() => {
	vi.resetAllMocks();
	// A protected app page: not an auth page, not a share page, so the layout's
	// load effect is allowed to fire.
	page.url = new URL('http://localhost/alice/ws');
	api.workspaces.list.mockResolvedValue([U1_WS]);
	api.auth.session.mockResolvedValue(sessionFor('u1'));
});

afterEach(() => {
	cleanup();
	authStore.clear();
});

describe('BUG-2991: the root layout refetches workspaces when the user changes', () => {
	it('re-arms its one-shot load latch on an identity change', async () => {
		render(Layout, { props: { children: childSnippet } });
		await settle();

		// Non-vacuity: the latch has to have been SET for re-arming it to mean
		// anything, and the list has to have been loaded for the reset to have
		// something to clear.
		expect(api.workspaces.list).toHaveBeenCalledTimes(1);
		expect(workspaceStore.workspaces).toHaveLength(1);

		// A different user signs in. The store's own listener empties the list;
		// this layout's listener re-arms the latch so the effect asks again.
		//
		// The second response is DIFFERENT DATA, deliberately (codex round 2).
		// A call count alone is a weak oracle: returning the same fixture twice
		// would let this pass even if the second call committed nothing, or
		// committed the first user's list. Asserting the store ends up holding
		// u2's workspace is the claim that actually matters.
		api.workspaces.list.mockResolvedValue([U2_WS]);
		api.auth.session.mockResolvedValue(sessionFor('u2'));
		await authStore.load();
		await settle();

		expect(api.workspaces.list).toHaveBeenCalledTimes(2);
		expect(workspaceStore.workspaces).toEqual([U2_WS]);
	});

	it('does NOT refetch when the session refetch returns the same user', async () => {
		// The counterfactual. Re-arming unconditionally would turn every routine
		// session refetch into a second list request.
		render(Layout, { props: { children: childSnippet } });
		await settle();
		expect(api.workspaces.list).toHaveBeenCalledTimes(1);

		await authStore.load();
		await settle();

		expect(api.workspaces.list).toHaveBeenCalledTimes(1);
		expect(workspaceStore.workspaces).toEqual([U1_WS]);
	});

	it('refetches after a real sign-out and sign-in as someone else', async () => {
		// The logout path specifically, which the first case skips by going
		// straight from one user to another.
		render(Layout, { props: { children: childSnippet } });
		await settle();
		expect(api.workspaces.list).toHaveBeenCalledTimes(1);

		authStore.clear();
		await settle();
		expect(workspaceStore.workspaces).toEqual([]);

		api.workspaces.list.mockResolvedValue([U2_WS]);
		api.auth.session.mockResolvedValue(sessionFor('u2'));
		await authStore.load();
		await settle();

		expect(workspaceStore.workspaces).toEqual([U2_WS]);
	});
});

describe('BUG-3005: which identity transitions reload the tab', () => {
	// The reload exists so one user's data cannot be visible to the next. Which
	// transitions need it is therefore a question about whose data is being
	// dropped, and an ANONYMOUS baseline holds nobody's private data — so a
	// sign-IN does not reload. Reloading there also put a full page load in the
	// middle of the login flow, racing its own navigation; the E2E suite caught
	// that, which is why these three legs exist.
	//
	// EACH LEG ASSERTS AN EXACT CALL COUNT, and that is load-bearing rather than
	// style: `afterEach` clears the store, so every test starts from an
	// ESTABLISHED empty identity and its own setup performs a sign-in. Removing
	// the guard therefore adds a reload during setup and all three legs fail —
	// the swap and sign-out ones on the COUNT rather than on the absence.

	it('reloads on a SWAP from one user to another', async () => {
		render(Layout, { props: { children: childSnippet } });
		await settle();
		// PRECONDITION: u1 is established, so what follows is a change rather
		// than a baseline.
		expect(authStore.userId).toBe('u1');
		expect(identityReload.reloadForIdentityChange).not.toHaveBeenCalled();

		api.auth.session.mockResolvedValue(sessionFor('u2'));
		await authStore.load();
		await settle();

		expect(identityReload.reloadForIdentityChange).toHaveBeenCalledTimes(1);
	});

	it('reloads on SIGN-OUT', async () => {
		render(Layout, { props: { children: childSnippet } });
		await settle();
		expect(authStore.userId).toBe('u1');

		authStore.clear();
		await settle();

		expect(identityReload.reloadForIdentityChange).toHaveBeenCalledTimes(1);
	});

	it('does NOT reload on a SIGN-IN from an unauthenticated tab', async () => {
		// The leg the E2E failure is about. `collab-helpers.ts` signs in from
		// inside a mounted page, and a reload there takes the page out from
		// under whatever is driving it — in a test, and in the real login flow.
		api.auth.session.mockResolvedValue(null);
		render(Layout, { props: { children: childSnippet } });
		await settle();
		// PRECONDITION: the tab is unauthenticated and that baseline is
		// established, so the sign-in below IS a transition and this leg is not
		// passing because nothing fired.
		expect(authStore.userId).toBe('');

		api.auth.session.mockResolvedValue(sessionFor('u1'));
		await authStore.load();
		await settle();

		expect(authStore.userId).toBe('u1');
		expect(identityReload.reloadForIdentityChange).not.toHaveBeenCalled();
	});
});
