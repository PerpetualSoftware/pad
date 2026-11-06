// TASK-4315 PR 3b: opening and closing Chat over a route must not remount that
// route's page. A mount/destroy counter on the child proves it; the editor, caret
// and scroll retention that follows from it is proven in e2e/chat-shell.spec.ts.
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
	invalidateAll: vi.fn(),
	pushState: vi.fn(),
	replaceState: vi.fn(),
}));

import Layout from './+layout.svelte';
import { authStore } from '$lib/stores/auth.svelte';
import { chatInbox } from '$lib/stores/chatInbox.svelte';
import { page } from '$app/state';

const counts = { mounted: 0, destroyed: 0 };
const childSnippet = createRawSnippet(() => ({
	render: () => `<div data-testid="child">child</div>`,
	setup: () => {
		counts.mounted++;
		return () => {
			counts.destroyed++;
		};
	},
}));

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
	vp.mobile = false;
	counts.mounted = 0;
	counts.destroyed = 0;
	setWidth(1600);
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
	document.body.classList.remove('has-mobile-topbar');
	page.params = {};
	closeChat();
	vi.clearAllMocks();
});

describe('the Chat host in the root layout', () => {
	it.each([
		['a workspace route', '/alice/ws/tasks', { username: 'alice', workspace: 'ws' }],
		['the console', '/console', {}],
	])('opening and closing Chat over %s does not remount the child', async (_n, path, params) => {
		await mountAt(path, params);
		expect(counts).toEqual({ mounted: 1, destroyed: 0 });
		expect(document.querySelector('[data-chat-host]')).toBeNull();

		openChat('/messages/3');
		await settle();
		expect(document.querySelector('[data-chat-host]')).not.toBeNull();
		expect(document.querySelector('[data-testid="child"]')).not.toBeNull();
		expect(counts).toEqual({ mounted: 1, destroyed: 0 });

		openChat('/messages');
		await settle();
		closeChat();
		await settle();
		expect(document.querySelector('[data-chat-host]')).toBeNull();
		expect(counts).toEqual({ mounted: 1, destroyed: 0 });
	});

	it('over a workspace route the sidebar and top bar stay, and the host is outside every inert region', async () => {
		await mountAt('/alice/ws/tasks', { username: 'alice', workspace: 'ws' });
		openChat();
		setWidth(1100); // a drawer: it covers the page, so the page goes inert
		await settle();
		const host = document.querySelector('[data-chat-host]')!;
		expect(document.querySelector('aside')).not.toBeNull();
		expect(document.querySelector('header.topbar')).not.toBeNull();
		expect(inertAncestor(host)).toBeNull();
		expect((document.querySelector('main.main-content') as HTMLElement).inert).toBe(true);
	});

	it('the console child sits in an inert wrapper while a drawer covers it, and the host does not', async () => {
		await mountAt('/console');
		openChat();
		setWidth(900); // the console has no sidebar to subtract
		await settle();
		expect(inertAncestor(document.querySelector('[data-testid="child"]'))).not.toBeNull();
		expect(inertAncestor(document.querySelector('[data-chat-host]'))).toBeNull();
	});

	it('a docked Chat leaves the page live', async () => {
		await mountAt('/alice/ws/tasks', { username: 'alice', workspace: 'ws' });
		openChat();
		await settle();
		expect(document.querySelector('[role="complementary"]')).not.toBeNull();
		expect((document.querySelector('main.main-content') as HTMLElement).inert).toBe(false);
	});

	it('phone: no host, however the page state looks', async () => {
		vp.mobile = true;
		await mountAt('/alice/ws/tasks', { username: 'alice', workspace: 'ws' });
		expect(document.querySelector('[data-chat-host]')).toBeNull();
		expect(document.querySelector('a[aria-label^="Chat"].chat-button')).toBeNull();
	});
});
