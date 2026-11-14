// TASK-4044 PR 2a-ii: the Chat row in the desktop user menu.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick, flushSync } from 'svelte';

const h = vi.hoisted(() => ({ chatOn: true }));
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
		authenticated: true,
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

vi.mock('$lib/stores/chatSetting.svelte', () => ({
	chatSetting: {
		get enabled() {
			return h.chatOn;
		}
	}
}));

import TopBar from './TopBar.svelte';

const trigger = () => document.querySelector<HTMLButtonElement>('.user-trigger')!;
const row = () => document.querySelector<HTMLAnchorElement>('a.menu-link[href="/messages"]');

async function settle() {
	for (let i = 0; i < 5; i++) await Promise.resolve();
	await tick();
	flushSync();
}

beforeEach(() => {
	cleanup();
	h.chatOn = true;
});
afterEach(() => cleanup());


describe('desktop TopBar (TASK-4545)', () => {
	it('has no discovery or Chat entry and has the approved account menu', async () => {
		render(TopBar);
		await settle();
		expect(document.querySelector('.workspace-add')).toBeNull();
		expect(document.querySelector('.chat-button')).toBeNull();
		trigger().click(); await settle();
		expect(row()).toBeNull();
		const labels = [...document.querySelectorAll('[role="menuitem"]')].map(el => el.textContent?.trim());
		expect(labels).toEqual(['Settings', 'Light mode', 'Keyboard shortcuts', 'Docs', 'Changelog', 'GitHub', 'Connect a project…', 'Sign out']);
		expect(document.querySelector('.user-dropdown-email')?.textContent).toBe('ada@example.com');
	});
	it('keeps the phone account menu', async () => {
		render(TopBar, { mobile: true }); await settle();
		trigger().click(); await settle();
		expect(row()).not.toBeNull();
	});
});

it('the phone bar is unchanged: no Chat button', async () => {
	render(TopBar, { props: { mobile: true } });
	await settle();
	expect(document.querySelector('header.topbar a.chat-button')).toBeNull();
});
