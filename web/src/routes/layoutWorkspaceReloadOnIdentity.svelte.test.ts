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
	api.workspaces.list.mockResolvedValue([{ id: 'w1', slug: 'ws', name: 'WS' }]);
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
		api.auth.session.mockResolvedValue(sessionFor('u2'));
		await authStore.load();
		await settle();

		expect(api.workspaces.list).toHaveBeenCalledTimes(2);
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
	});
});
