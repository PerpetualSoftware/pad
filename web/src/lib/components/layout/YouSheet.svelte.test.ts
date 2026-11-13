// TASK-4044 PR 2a-ii: the Chat row in the mobile You sheet.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick, flushSync } from 'svelte';

const h = vi.hoisted(() => ({ chatOn: true, path: '/console' }));
vi.mock('$app/navigation', () => ({ goto: vi.fn(), afterNavigate: vi.fn() }));
vi.mock('$app/state', () => ({
	page: {
		get url() {
			return new URL(`http://localhost${h.path}`);
		},
		state: {}
	}
}));
vi.mock('$lib/api/client', () => ({ api: { auth: { logout: vi.fn() } } }));
vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: { current: null } }));
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

vi.mock('$lib/stores/chatSetting.svelte', () => ({
	chatSetting: {
		get enabled() {
			return h.chatOn;
		}
	}
}));

import YouSheet from './YouSheet.svelte';
import { chatStore } from '$lib/stores/chat.svelte';

beforeEach(() => {
	cleanup();
	h.chatOn = true;
	h.path = '/console';
	chatStore.unreadTotal = 4;
});
afterEach(() => {
	cleanup();
	chatStore.unreadTotal = 0;
});

describe('YouSheet chat row (TASK-4044)', () => {
	it('has a Chat row to /messages with the unread count', async () => {
		render(YouSheet, { props: { open: true, onclose: vi.fn() } });
		await tick();
		flushSync();
		const a = document.querySelector<HTMLAnchorElement>('.rows a[href="/messages"]');
		expect(a).not.toBeNull();
		expect(a?.classList.contains('row')).toBe(true);
		expect(a?.getAttribute('aria-label')).toBe('Chat, 4 unread');
	});

	it('has no Chat row while the setting is off (TASK-4315)', async () => {
		h.chatOn = false;
		render(YouSheet, { props: { open: true, onclose: vi.fn() } });
		await tick();
		flushSync();
		expect(document.querySelector('.rows a[href="/console/settings"]')).not.toBeNull();
		expect(document.querySelector('.rows a[href="/messages"]')).toBeNull();
	});

	it('updates the Chat row badge live', async () => {
		render(YouSheet, { props: { open: true, onclose: vi.fn() } });
		await tick();
		flushSync();
		const a = document.querySelector<HTMLAnchorElement>('.rows a[href="/messages"]')!;
		chatStore.unreadTotal = 7;
		flushSync();
		expect(a.querySelector('.unread-badge')?.textContent).toBe('7');
		expect(a.getAttribute('aria-label')).toBe('Chat, 7 unread');
		chatStore.unreadTotal = 0;
		flushSync();
		expect(a.querySelector('.unread-badge')).toBeNull();
		expect(a.getAttribute('aria-label')).toBe('Chat');
	});

	it('closes the sheet when tapped', async () => {
		const onclose = vi.fn();
		render(YouSheet, { props: { open: true, onclose } });
		await tick();
		flushSync();
		const a = document.querySelector<HTMLAnchorElement>('.rows a[href="/messages"]')!;
		a.addEventListener('click', (e) => e.preventDefault());
		a.click();
		expect(onclose).toHaveBeenCalledTimes(1);
	});
});

describe('YouSheet agenda row (TASK-4526)', () => {
	it('links the Agenda and closes the sheet on tap', async () => {
		const onclose = vi.fn();
		render(YouSheet, { props: { open: true, onclose } });
		await tick();
		flushSync();
		const a = document.querySelector<HTMLAnchorElement>('.rows a[href="/console/agenda"]');
		expect(a).not.toBeNull();
		expect(a?.classList.contains('row')).toBe(true);
		expect(a?.textContent).toContain('Agenda');
		expect(a?.classList.contains('active')).toBe(false);
		expect(a?.hasAttribute('aria-current')).toBe(false);
		a!.addEventListener('click', (e) => e.preventDefault());
		a!.click();
		expect(onclose).toHaveBeenCalledTimes(1);
	});

	it('shows as the current page on the agenda route', async () => {
		h.path = '/console/agenda';
		render(YouSheet, { props: { open: true, onclose: vi.fn() } });
		await tick();
		flushSync();
		const a = document.querySelector<HTMLAnchorElement>('.rows a[href="/console/agenda"]')!;
		expect(a.classList.contains('active')).toBe(true);
		expect(a.getAttribute('aria-current')).toBe('page');
	});
});
