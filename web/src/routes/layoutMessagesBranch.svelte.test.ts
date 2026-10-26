// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// TASK-4044: /messages (the chat inbox) renders without the workspace shell.
// The root layout is the shell for every other route, so this pins both halves:
// /messages and /messages/3 get no Sidebar and no TopBar, and a workspace route,
// /console and /messagesfoo still do.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { createRawSnippet, tick } from 'svelte';

const api = vi.hoisted(() => ({
	auth: { session: vi.fn() },
	workspaces: { list: vi.fn() },
	chat: { unread: vi.fn() },
}));
const vp = vi.hoisted(() => ({ mobile: false }));

vi.mock('$lib/api/client', () => {
	const never = () => new Promise(() => {});
	return {
		api: new Proxy(api, {
			get: (t, k) => (k in t ? (t as never)[k] : new Proxy({}, { get: () => never })),
		}),
		setAccessRevokedHandler: () => {},
		setIdentityProvider: () => {},
		setRateLimitHandler: () => {},
		setNetworkUnreachableHandler: () => {},
		isPlanLimitError: () => false,
		planLimitMessage: () => '',
	};
});
vi.mock('$lib/stores/identityReload.svelte', () => ({
	reloadForIdentityChange: vi.fn(),
	clearPersistentIdentityState: vi.fn(),
}));
vi.mock('$lib/services/accessStream.svelte', () => ({ accessStream: { start: vi.fn() } }));
vi.mock('$lib/stores/breakpoint.svelte', () => ({
	viewport: {
		get isMobile() {
			return vp.mobile;
		},
		get isCoarsePointer() {
			return false;
		},
	},
	MOBILE_MEDIA_QUERY: '(max-width: 768px)',
}));
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

const childSnippet = createRawSnippet(() => ({ render: () => `<div data-testid="child">child</div>` }));

async function settle(): Promise<void> {
	for (let i = 0; i < 10; i++) {
		await Promise.resolve();
		await tick();
	}
}

async function mountAt(path: string, params: Record<string, string> = {}) {
	page.url = new URL(`http://localhost${path}`);
	page.params = params;
	render(Layout, { props: { children: childSnippet } });
	await settle();
}

const hasSidebar = () => document.querySelector('aside') !== null;
const hasTopBar = () => document.querySelector('header.topbar') !== null;
const hasChild = () => document.querySelector('[data-testid="child"]') !== null;

beforeEach(() => {
	vp.mobile = false;
	api.auth.session.mockResolvedValue({ authenticated: true, user: { id: 'u1', email: 'u1@example.com' } });
	api.workspaces.list.mockResolvedValue([]);
	api.chat.unread.mockResolvedValue({ conversations: [], total: 0 });
});

afterEach(() => {
	cleanup();
	authStore.clear();
	document.body.classList.remove('has-mobile-topbar');
	page.params = {};
	vi.clearAllMocks();
});

describe('TASK-4044: the root layout gives /messages no workspace shell', () => {
	it('control: a workspace route renders the Sidebar and the TopBar', async () => {
		await mountAt('/alice/ws', { username: 'alice', workspace: 'ws' });
		expect({ child: hasChild(), sidebar: hasSidebar(), topbar: hasTopBar() }).toEqual({
			child: true,
			sidebar: true,
			topbar: true,
		});
	});

	it('control: /console renders its child and not the app shell', async () => {
		await mountAt('/console');
		expect({ child: hasChild(), sidebar: hasSidebar(), topbar: hasTopBar() }).toEqual({
			child: true,
			sidebar: false,
			topbar: false,
		});
	});

	it('control: a non-workspace shell page (/) still renders the Sidebar and the TopBar', async () => {
		await mountAt('/');
		expect({ sidebar: hasSidebar(), topbar: hasTopBar() }).toEqual({ sidebar: true, topbar: true });
	});

	it.each(['/messages', '/messages/3'])('%s renders its child with no Sidebar and no TopBar', async (path) => {
		await mountAt(path);
		expect({ child: hasChild(), sidebar: hasSidebar(), topbar: hasTopBar() }).toEqual({
			child: true,
			sidebar: false,
			topbar: false,
		});
	});

	it('/messagesfoo is not the chat inbox: it keeps the shell', async () => {
		await mountAt('/messagesfoo');
		expect({ sidebar: hasSidebar(), topbar: hasTopBar() }).toEqual({ sidebar: true, topbar: true });
	});

	it('/messages keeps the toast surface', async () => {
		await mountAt('/messages');
		expect(document.querySelector('.toast-container [data-toast-region="polite"]')).not.toBeNull();
	});

	it('has-mobile-topbar is on for a mobile shell page and off on /messages', async () => {
		vp.mobile = true;
		await mountAt('/');
		expect(document.body.classList.contains('has-mobile-topbar')).toBe(true);
		cleanup();
		document.body.classList.remove('has-mobile-topbar');

		await mountAt('/messages');
		expect(document.body.classList.contains('has-mobile-topbar')).toBe(false);
	});
});
