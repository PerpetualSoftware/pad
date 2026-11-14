import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';
import { tick } from 'svelte';

/**
 * BUG-4472. A workspace the signed-in user cannot open answers the dashboard
 * fetch with 404 `not_found` (the workspace-access middleware answers the same
 * for "does not exist" and "not a member"). That is terminal (BUG-2025), but
 * the page used to render only "No dashboard data available.", and on mobile
 * no other chrome renders either (BottomNav needs a resolved workspace), so the
 * user had no way out. The page must say what happened and link to the
 * caller's workspace list.
 */

vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		identityEpoch: 0,
		userId: 'u1',
		user: { id: 'u1', name: 'A', email: 'a@example.com' },
		mcpPublicUrl: '',
		onIdentityChange: () => () => {},
	},
}));

type Held = { resolve: (v: unknown) => void; reject: (e: unknown) => void };
const dashboardCalls = vi.hoisted(() => [] as Held[]);

vi.mock('$lib/api/client', () => ({
	api: {
		dashboard: {
			get: vi.fn(() => new Promise((resolve, reject) => {
				dashboardCalls.push({ resolve: resolve as (v: unknown) => void, reject });
			})),
		},
		collections: { list: vi.fn(async () => []) },
		workspaces: { claimCode: vi.fn(async () => ({ code: 'x', expires_at: '' })) },
	},
	PadApiError: class PadApiError extends Error {
		code: string;
		constructor(code: string) { super(code); this.code = code; }
	},
}));

vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: {
		setCurrent: vi.fn(async () => {}),
		get current() { return null; },
		get isOwner() { return false; },
		get membershipKnown() { return true; },
		get currentRole() { return null; },
		get workspaces() { return []; },
		loadAll: vi.fn(async () => {}),
	},
}));
const syncCallbacks = vi.hoisted(() => [] as Array<(r: unknown) => void>);
vi.mock('$lib/services/sync.svelte', () => ({
	syncService: {
		onSync: (cb: (r: unknown) => void) => { syncCallbacks.push(cb); return () => {}; },
		start: () => {},
		stop: () => {},
	},
}));
vi.mock('$lib/stores/ui.svelte', () => ({
	uiStore: {
		get connectAfterNavigateSlug() { return null; },
		consumeConnectAfterNavigate() {},
		requestQuickAdd() {},
	},
}));
vi.mock('$lib/stores/collections.svelte', () => ({ collectionStore: { loadCollections: vi.fn() } }));
vi.mock('$lib/stores/title.svelte', () => ({ titleStore: { setPageTitle() {} } }));
vi.mock('$lib/scroll/restore.svelte', () => ({
	createScrollRestoration: () => ({ snapshot: { capture: () => null, restore: () => {} } }),
}));

import { PadApiError } from '$lib/api/client';
import { page } from '$app/state';
import DashboardPage from './+page.svelte';

async function spin(until: () => boolean, what: string, rounds = 60): Promise<void> {
	for (let i = 0; i < rounds; i++) {
		if (until()) return;
		await Promise.resolve();
		await tick();
	}
	if (!until()) throw new Error(what);
}

const shown = (text: string) => document.body.textContent?.includes(text) ?? false;

async function mountAndFail(err: unknown): Promise<void> {
	page.params = { username: 'dave', workspace: 'ws' };
	page.url = new URL('http://localhost/dave/ws');
	render(DashboardPage);
	await spin(() => dashboardCalls.length > 0, 'the page never fetched the dashboard');
	dashboardCalls[0]!.reject(err);
	await spin(() => !document.querySelector('.skeleton-dashboard'), 'the page never left its loading state');
}

describe('a workspace the caller cannot open (BUG-4472)', () => {
	beforeEach(() => {
		dashboardCalls.length = 0;
		syncCallbacks.length = 0;
	});
	afterEach(() => cleanup());

	it('says the workspace is unavailable and links to the caller\'s workspaces', async () => {
		await mountAndFail(new PadApiError('not_found'));
		expect(shown('Workspace not found')).toBe(true);
		expect(shown("doesn't exist, or a@example.com doesn't have access to it")).toBe(true);
		const link = document.querySelector<HTMLAnchorElement>('a[href="/"]');
		expect(link, 'no way out to the workspace list').not.toBeNull();
		expect(link!.textContent).toContain('Go to your workspaces');
		expect(shown('No dashboard data available.')).toBe(false);
		// Terminal, not transient: no Retry loop that can never succeed.
		expect(document.querySelector('.dash-error')).toBeNull();
	});

	it('a later background reload that fails transiently does not turn it into a Retry', async () => {
		await mountAndFail(new PadApiError('not_found'));
		expect(syncCallbacks.length, 'the page did not subscribe to sync').toBeGreaterThan(0);
		for (const cb of syncCallbacks) cb({ workspace: 'ws', type: 'changed' });
		await spin(() => dashboardCalls.length > 1, 'the sync signal never reloaded — this leg measures nothing');
		dashboardCalls[1]!.reject(new Error('blip'));
		for (let i = 0; i < 5; i++) { await Promise.resolve(); await tick(); }
		expect(shown('Workspace not found')).toBe(true);
		expect(document.querySelector('.dash-error')).toBeNull();
	});

	it('CONTROL: a transient failure still shows Retry, not the unavailable state', async () => {
		await mountAndFail(new Error('boom'));
		expect(document.querySelector('.dash-error')).not.toBeNull();
		expect(shown('Workspace not found')).toBe(false);
		expect(document.querySelector('a[href="/"]')).toBeNull();
	});
});
