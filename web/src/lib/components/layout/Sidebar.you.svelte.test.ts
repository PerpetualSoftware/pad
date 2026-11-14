import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';

const h = vi.hoisted(() => ({
	ws: { current: { slug: 'ws', owner_username: 'u', is_guest: false } as { slug: string; owner_username: string; is_guest: boolean } },
	auth: { authenticated: true },
	chatOn: true,
	opened: [] as string[]
}));
vi.mock('$lib/stores/chatSurface.svelte', () => ({
	chatSurface: {
		isOpen: false,
		handleLinkClick: (e: MouseEvent, href: string) => {
			if (e.ctrlKey) return;
			e.preventDefault();
			h.opened.push(href);
		}
	}
}));

vi.mock('$lib/api/client', () => ({
	api: {
		health: vi.fn(async () => ({ version: 'dev', commit: 'abcdef0' })),
		needs: { list: vi.fn(async () => ({ rows: [], total: 7, hidden: 0, workspaces: [] })) },
	getMyAgenda: vi.fn(async () => ({ entries: [{ type: 'date', day: new Date().toISOString().slice(0,10) }], priority_list: [] })),
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
		workspaces: [{ id: 'one', slug: 'one', name: 'First', owner_username: 'u' }, { id: 'two', slug: 'two', name: 'Last', owner_username: 'u' }],
	canEditCollection: () => true,
		membershipKnown: true
	}
}));
vi.mock('$lib/stores/collections.svelte', async () => await import('./sidebarQuickAdd.fixture.svelte'));
vi.mock('$lib/stores/chatSetting.svelte', () => ({
	chatSetting: {
		get enabled() {
			return h.chatOn;
		}
	}
}));
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


import { page } from '$app/state';
import { lastWorkspace } from '$lib/stores/lastWorkspace.svelte';
import { screen } from '@testing-library/svelte';
async function settle() { for(let i=0;i<10;i++) await tick(); }
beforeEach(() => { cleanup(); lastWorkspace.clear(); h.chatOn = true; h.auth.authenticated = true; page.url = new URL('http://localhost/'); page.params = {}; });
it('pins the four You destinations in order with counts', async () => {
	render(Sidebar); await settle();
	const nav = screen.getByRole('navigation', { name: 'You' });
	expect([...nav.querySelectorAll('a')].map(a => a.getAttribute('href'))).toEqual(['/', '/needs', '/agenda', '/messages']);
	expect(nav.textContent).toContain('7'); expect(nav.textContent).toContain('1 today'); expect(nav.textContent).toContain('4');
	expect(document.querySelector('.collection-nav a[href="/messages"]')).toBeNull();
});
it.each(['/', '/needs', '/agenda', '/messages'])('highlights only the You route %s', async path => {
	page.url = new URL('http://localhost' + path); render(Sidebar); await settle();
	expect(document.querySelector('nav[aria-label="You"] a.active')?.getAttribute('href')).toBe(path);
	expect(document.querySelector('.collection-nav .active')).toBeNull();
});
it('hides Chat when disabled', async () => {
	h.chatOn = false; render(Sidebar); await settle();
	expect(document.querySelector('nav[aria-label="You"] a[href="/messages"]')).toBeNull();
});
it('retains the last workspace on Home without a workspace highlight', async () => {
	lastWorkspace.note('u', 'two'); render(Sidebar); await settle();
	expect(screen.getByRole('button', { name: /Last/ })).toBeTruthy();
	expect(document.querySelector('a[href="/u/two"]')).not.toBeNull();
	expect(document.querySelector('.collection-nav .active')).toBeNull();
});
it('falls back to the first workspace', async () => {
	render(Sidebar); await settle();
	expect(screen.getByRole('button', { name: /First/ })).toBeTruthy();
});
