// TASK-4315 PR 4: the phone bars in the root layout. Outside a workspace the compact
// GlobalBottomBar serves signed-in pages; the Chat sheet covers the page but never the
// bar; <main> is only left live for the sheet when a workspace's own BottomNav is in it.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { createRawSnippet, tick, flushSync } from 'svelte';

const api = vi.hoisted(() => ({
	auth: { session: vi.fn(), setTimezoneIfUnset: vi.fn(async () => ({ timezone: 'UTC' })) },
	workspaces: { list: vi.fn() },
	chat: { unread: vi.fn() },
}));
const vp = vi.hoisted(() => ({ mobile: false }));

vi.mock('$app/state', async () => await import('../test/mocks/app-state-reactive.svelte'));
vi.mock('$lib/api/client', () => {
	const never = () => new Promise(() => {});
	return {
		api: new Proxy(api, {
			get: (t, k) => (k in t ? (t as never)[k] : new Proxy({}, { get: () => never })),
		}),
		setAccessRevokedHandler: () => {},
		setIdentityProvider: () => {},
		setReturnPathProvider: () => {},
		setRateLimitHandler: () => {},
		setDatabaseBusyHandler: () => {},
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
	onNavigate: () => {},
	invalidateAll: vi.fn(),
	pushState: vi.fn(),
	replaceState: vi.fn(),
}));

import Layout from './+layout.svelte';
import { authStore } from '$lib/stores/auth.svelte';
import { chatInbox } from '$lib/stores/chatInbox.svelte';
import { page } from '$app/state';

const childSnippet = createRawSnippet(() => ({ render: () => `<div data-testid="child">child</div>` }));

async function settle(): Promise<void> {
	for (let i = 0; i < 10; i++) {
		await Promise.resolve();
		await tick();
		flushSync();
	}
}

function setWidth(w: number) {
	Object.defineProperty(window, 'innerWidth', { configurable: true, writable: true, value: w });
	window.dispatchEvent(new Event('resize'));
}

async function mountAt(path: string, params: Record<string, string> = {}) {
	page.url = new URL(`http://localhost${path}`);
	page.params = params;
	render(Layout, { props: { children: childSnippet } });
	await settle();
}

// jsdom has no `inert` attribute reflection: Svelte assigns the property.
const inertAncestor = (el: Element | null): Element | null => {
	for (let n = el; n; n = n.parentElement) if ((n as HTMLElement).inert) return n;
	return null;
};

const openChat = (path = '/messages') => {
	page.state = { chatDepth: 1 };
	page.shallow = { url: new URL(`http://localhost${path}`), route: null, params: null };
};
const closeChat = () => {
	page.state = {};
	page.shallow = null;
};

beforeEach(() => {
	vp.mobile = true;
	setWidth(390);
	page.shallow = null;
	page.state = {};
	api.auth.session.mockResolvedValue({
		authenticated: true,
		user: { id: 'u1', email: 'u1@example.com', chat_enabled: true },
	});
	api.workspaces.list.mockResolvedValue([]);
	api.chat.unread.mockResolvedValue({ conversations: [], total: 0 });
});

afterEach(() => {
	cleanup();
	authStore.clear();
	chatInbox.stop();
	chatInbox.stopPresence();
	document.body.classList.remove('has-mobile-topbar', 'has-bottom-nav');
	page.params = {};
	closeChat();
	vi.clearAllMocks();
});

const bar = () => document.querySelector('nav.bottom-nav[aria-label="Primary"]');

describe('the compact bar outside a workspace', () => {
	it.each([
		['account settings', '/settings/account'],
		['the workspace picker', '/'],
		['a standalone /messages', '/messages'],
		['a standalone thread', '/messages/3'],
	])('is shown on a phone at %s', async (_n, path) => {
		await mountAt(path);
		expect(bar()).not.toBeNull();
		expect([...bar()!.querySelectorAll('.bn-label')].map((e) => e.textContent)).toEqual(['Workspaces', 'Chat', 'You']);
		expect(document.body.classList.contains('has-bottom-nav')).toBe(true);
	});

	it.each([
		['login', '/login'],
		['a share page', '/s/abc'],
	])('is not shown on %s', async (_n, path) => {
		await mountAt(path);
		expect(bar()).toBeNull();
	});

	it('is not shown on a wide screen', async () => {
		vp.mobile = false;
		setWidth(1600);
		await mountAt('/');
		expect(bar()).toBeNull();
	});

	it('is not shown signed out', async () => {
		api.auth.session.mockResolvedValue({ authenticated: false });
		await mountAt('/');
		expect(bar()).toBeNull();
	});

	it('is not shown inside a workspace (BottomNav owns it there)', async () => {
		await mountAt('/alice/ws/tasks', { username: 'alice', workspace: 'ws' });
		expect(bar()).toBeNull();
	});

	it.each([
		['account settings', '/settings/account'],
		['the workspace picker', '/'],
	])('stays outside every inert region while the Chat sheet covers %s', async (_n, path) => {
		await mountAt(path);
		openChat();
		await settle();
		expect(document.querySelector('.ds-panel')).not.toBeNull();
		expect(inertAncestor(bar())).toBeNull();
		expect(inertAncestor(document.querySelector('.ds-panel'))).toBeNull();
		expect(inertAncestor(document.querySelector('[data-testid="child"]'))).not.toBeNull();
	});
});

describe('<main> under the phone Chat sheet', () => {
	it('is inert outside a workspace even though the sheet keeps a bottom nav live', async () => {
		await mountAt('/messagesfoo');
		openChat();
		await settle();
		expect((document.querySelector('main.main-content') as HTMLElement).inert).toBe(true);
		closeChat();
		await settle();
		expect((document.querySelector('main.main-content') as HTMLElement).inert).toBe(false);
	});

	it('stays live inside a workspace, where the nav is in it', async () => {
		await mountAt('/alice/ws/tasks', { username: 'alice', workspace: 'ws' });
		openChat();
		await settle();
		expect(document.querySelector('.ds-panel')).not.toBeNull();
		expect((document.querySelector('main.main-content') as HTMLElement).inert).toBe(false);
	});
});
