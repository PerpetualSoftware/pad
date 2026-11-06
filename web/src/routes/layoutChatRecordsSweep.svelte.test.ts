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
	// BUG-2983 added this seam; the layout calls it at module scope, so a mock
	// without it throws before any assertion runs.
	setIdentityProvider: () => {},
	setRateLimitHandler: () => {},
	setDatabaseBusyHandler: () => {},
	setNetworkUnreachableHandler: () => {},
	isPlanLimitError: () => false,
	planLimitMessage: () => '',
}));

// `goto` must not run: the unauthenticated branches call it, and jsdom has no
// navigation. Nothing here asserts on it.
const identityReload = vi.hoisted(() => ({
	reloadForIdentityChange: vi.fn(),
	clearPersistentIdentityState: vi.fn(),
}));
vi.mock('$lib/stores/identityReload.svelte', () => identityReload);

// The access stream (TASK-3275; elected per browser since BUG-3318) is started by this layout. jsdom has
// no EventSource, and the stream's behaviour is pinned in its own test; here
// only the binding is observable, so it is a spy.
const accessStream = vi.hoisted(() => ({ start: vi.fn() }));
vi.mock('$lib/services/accessStream.svelte', () => ({ accessStream }));

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
import { page } from '$app/state';

// TASK-4315 unit 2: the root layout sweeps chat records that belong to another identity.
// A reload after session expiry starts anonymous and must keep them; the first time an
// identity is ESTABLISHED in a tab (which fires no change event) must purge the others'.
const childSnippet = createRawSnippet(() => ({ render: () => `<div>child</div>` }));

function sessionFor(id: string) {
	return { authenticated: true, user: { id, email: `${id}@example.com` } };
}
function seed(userId: string, n: number) {
	sessionStorage.setItem(`pad:chat-draft:${userId}:${n}`, JSON.stringify({ userId, n, text: `from ${userId}`, replySeq: null }));
}
async function settle(): Promise<void> {
	for (let i = 0; i < 10; i++) {
		await Promise.resolve();
		await tick();
	}
}

beforeEach(() => {
	vi.resetAllMocks();
	sessionStorage.clear();
	page.url = new URL('http://localhost/alice/ws');
	api.workspaces.list.mockResolvedValue([]);
});
afterEach(() => {
	cleanup();
	authStore.clear();
	sessionStorage.clear();
});

describe('TASK-4315: the root layout sweeps chat records of other identities', () => {
	it('keeps every record when the page loads anonymous (a session-expiry reload)', async () => {
		seed('uA', 1);
		api.auth.session.mockResolvedValue({ authenticated: false });
		render(Layout, { props: { children: childSnippet } });
		await settle();
		expect(sessionStorage.getItem('pad:chat-draft:uA:1')).not.toBeNull();
	});

	it('purges another identity\'s records and keeps the established user\'s', async () => {
		seed('uA', 1);
		seed('uB', 2);
		api.auth.session.mockResolvedValue(sessionFor('uB'));
		render(Layout, { props: { children: childSnippet } });
		await settle();
		expect(sessionStorage.getItem('pad:chat-draft:uA:1')).toBeNull();
		expect(sessionStorage.getItem('pad:chat-draft:uB:2')).not.toBeNull();
	});

	it('still sweeps when the first session load failed and an identity is established later', async () => {
		// authStore remembers across tests that an identity was established; this case needs a tab
		// whose FIRST resolution is the later one (which fires no change event), so load fresh modules.
		vi.resetModules();
		const fresh = await import('$lib/stores/auth.svelte');
		const { render: freshRender, cleanup: freshCleanup } = await import('@testing-library/svelte');
		const { createRawSnippet: freshSnippet, tick: freshTick } = await import('svelte');
		const child = freshSnippet(() => ({ render: () => `<div>child</div>` }));
		const { default: FreshLayout } = await import('./+layout.svelte');
		const { page: freshPage } = await import('$app/state');
		freshPage.url = new URL('http://localhost/alice/ws');
		seed('uA', 1);
		api.auth.session.mockRejectedValueOnce(new Error('offline'));
		const flush = async () => {
			for (let i = 0; i < 10; i++) {
				await Promise.resolve();
				await freshTick();
			}
		};
		freshRender(FreshLayout, { props: { children: child } });
		await flush();
		expect(sessionStorage.getItem('pad:chat-draft:uA:1')).not.toBeNull();
		api.auth.session.mockResolvedValue(sessionFor('uB'));
		await fresh.authStore.load();
		await flush();
		expect(sessionStorage.getItem('pad:chat-draft:uA:1')).toBeNull();
		freshCleanup();
		fresh.authStore.clear();
	});
});
