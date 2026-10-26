// TASK-4044 PR 2a-ii: the Chat row in the desktop user menu.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick, flushSync } from 'svelte';

vi.mock('$app/navigation', () => ({ goto: vi.fn(async () => {}), afterNavigate: vi.fn() }));
vi.mock('$lib/api/client', () => ({
	PadApiError: class extends Error {},
	api: {
		workspaces: {
			tabs: { list: vi.fn(), open: vi.fn(), close: vi.fn(), reorder: vi.fn(), update: vi.fn() },
			listDeleted: vi.fn(async () => [])
		}
	}
}));
vi.mock('$lib/stores/pendingInvitations.svelte', () => ({
	pendingInvitations: {
		count: 0,
		invitations: [],
		refresh: vi.fn(async () => {}),
		reserve: () => 0,
		set: () => {},
		remove: () => {}
	}
}));
vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: { current: null, workspaces: [] } }));
// The real store (other modules register identity listeners on it), with the
// fields these components read pinned.
vi.mock('$lib/stores/auth.svelte', async (orig) => {
	const real = await orig<typeof import('$lib/stores/auth.svelte')>();
	const fake = {
		user: { name: 'Ada', email: 'ada@example.com', role: 'member' },
		commerceAllowed: false,
		cloudMode: false,
		mcpPublicUrl: '',
		mcpAuth: ''
	};
	return {
		...real,
		authStore: new Proxy(real.authStore, {
			get: (t, k) => (k in fake ? fake[k as keyof typeof fake] : Reflect.get(t, k))
		})
	};
});
vi.mock('$lib/stores/chat.svelte', () => ({ chatStore: { unreadTotal: 2 } }));

import TopBar from './TopBar.svelte';

const trigger = () => document.querySelector<HTMLButtonElement>('.user-trigger')!;
const row = () => document.querySelector<HTMLAnchorElement>('a.menu-link[href="/messages"]');

async function settle() {
	for (let i = 0; i < 5; i++) await Promise.resolve();
	await tick();
	flushSync();
}

beforeEach(() => cleanup());
afterEach(() => cleanup());

describe('TopBar chat menu row (TASK-4044)', () => {
	it('is a menuitem in the user menu, with the count in its name', async () => {
		render(TopBar, { props: {} });
		await settle();
		expect(row()).toBeNull();
		trigger().click();
		await settle();
		expect(row()).not.toBeNull();
		expect(row()?.getAttribute('role')).toBe('menuitem');
		expect(row()?.getAttribute('aria-label')).toBe('Chat, 2 unread');
	});

	it('closes the menu when clicked', async () => {
		render(TopBar, { props: {} });
		await settle();
		trigger().click();
		await settle();
		const a = row()!;
		a.addEventListener('click', (e) => e.preventDefault());
		a.click();
		await settle();
		expect(trigger().getAttribute('aria-expanded')).toBe('false');
		expect(row()).toBeNull();
	});
});
