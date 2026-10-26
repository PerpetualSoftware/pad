// TASK-4044 PR 2a-ii: the Chat link in the sidebar footer. Chat is global, so
// it shows outside a workspace; a workspace guest does not get it (Q-f).
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';

type Ws = { slug: string; owner_username: string; is_guest: boolean } | null;
const ws = vi.hoisted(() => ({ current: null as Ws }));

vi.mock('$lib/api/client', () => ({
	api: {
		health: vi.fn(async () => ({ version: 'dev', commit: 'abcdef0' })),
		collections: { list: vi.fn(async () => []), update: vi.fn() },
	},
	isPlanLimitError: () => false,
	planLimitMessage: () => '',
}));
vi.mock('$app/navigation', () => ({ goto: vi.fn(), afterNavigate: vi.fn(), beforeNavigate: vi.fn() }));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: {
		get current() {
			return ws.current;
		},
		canEditCollection: () => true,
		membershipKnown: true,
	},
}));
vi.mock('$lib/stores/collections.svelte', async () => await import('./sidebarQuickAdd.fixture.svelte'));
vi.mock('$lib/stores/chat.svelte', () => ({ chatStore: { unreadTotal: 3 } }));

import Sidebar from './Sidebar.svelte';

const chat = () => document.querySelector<HTMLAnchorElement>('.footer-row a[href="/messages"]');

describe('Sidebar chat link (TASK-4044)', () => {
	beforeEach(() => cleanup());

	it('a member sees it, with the unread count', async () => {
		ws.current = { slug: 'ws', owner_username: 'u', is_guest: false };
		render(Sidebar);
		await tick();
		expect(chat()).not.toBeNull();
		expect(chat()?.getAttribute('aria-label')).toBe('Chat, 3 unread');
	});

	it('outside a workspace it still shows', async () => {
		ws.current = null;
		render(Sidebar);
		await tick();
		expect(chat()).not.toBeNull();
	});

	it('an in-workspace guest does not get it', async () => {
		ws.current = { slug: 'ws', owner_username: 'u', is_guest: true };
		render(Sidebar);
		await tick();
		expect(chat()).toBeNull();
	});
});
