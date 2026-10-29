import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';

const h = vi.hoisted(() => ({
	ws: { current: { slug: 'ws', owner_username: 'u', is_guest: false } as { slug: string; owner_username: string; is_guest: boolean } },
	auth: { authenticated: true }
}));

vi.mock('$lib/api/client', () => ({
	api: {
		health: vi.fn(async () => ({ version: 'dev', commit: 'abcdef0' })),
		collections: { list: vi.fn(async () => []), update: vi.fn() }
	},
	isPlanLimitError: () => false,
	planLimitMessage: () => ''
}));
vi.mock('$app/navigation', () => ({ goto: vi.fn(), afterNavigate: vi.fn(), beforeNavigate: vi.fn() }));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: {
		get current() {
			return h.ws.current;
		},
		canEditCollection: () => true,
		membershipKnown: true
	}
}));
vi.mock('$lib/stores/collections.svelte', async () => await import('./sidebarQuickAdd.fixture.svelte'));
vi.mock('$lib/stores/chat.svelte', () => ({ chatStore: { unreadTotal: 4 } }));
vi.mock('$lib/stores/auth.svelte', async (orig) => {
	const real = await orig<typeof import('$lib/stores/auth.svelte')>();
	return {
		...real,
		authStore: new Proxy(real.authStore, {
			get: (t, k) => (k === 'authenticated' ? h.auth.authenticated : Reflect.get(t, k))
		})
	};
});

import Sidebar from './Sidebar.svelte';

const entry = () => document.querySelector<HTMLAnchorElement>('nav.collection-nav a.nav-item[href="/messages"]');

// TASK-4044 3c (Option C, ruling 687eb810 Q-e): chat is per user, so the entry is
// gated on being signed in, not on the workspace's guest flag.
describe('Sidebar Chat entry (TASK-4044 3c)', () => {
	beforeEach(() => cleanup());

	it('a member sees it, after Tags, with the unread badge and a global href', async () => {
		h.ws.current = { slug: 'ws', owner_username: 'u', is_guest: false };
		h.auth.authenticated = true;
		render(Sidebar);
		await tick();
		const a = entry()!;
		expect(a).not.toBeNull();
		expect(a.getAttribute('href')).toBe('/messages');
		expect(a.getAttribute('aria-label')).toBe('Chat, 4 unread');
		expect(a.querySelector('.unread-badge')?.textContent).toBe('4');
		expect(a.previousElementSibling?.getAttribute('href')).toBe('/u/ws/tags');
	});

	it('a signed-in guest member of the workspace sees it too', async () => {
		h.ws.current = { slug: 'ws', owner_username: 'u', is_guest: true };
		h.auth.authenticated = true;
		render(Sidebar);
		await tick();
		expect(document.querySelector('a[href="/u/ws/tags"]')).toBeNull();
		expect(entry()).not.toBeNull();
	});

	it('a signed-out visitor does not', async () => {
		h.ws.current = { slug: 'ws', owner_username: 'u', is_guest: false };
		h.auth.authenticated = false;
		render(Sidebar);
		await tick();
		expect(document.querySelector('nav.collection-nav')).not.toBeNull();
		expect(entry()).toBeNull();
	});
});
