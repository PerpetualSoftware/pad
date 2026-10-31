// TASK-4044 PR 2a-ii: the Chat row in the mobile You sheet.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick, flushSync } from 'svelte';

vi.mock('$app/navigation', () => ({ goto: vi.fn(), afterNavigate: vi.fn() }));
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

import YouSheet from './YouSheet.svelte';
import { chatStore } from '$lib/stores/chat.svelte';

beforeEach(() => {
	cleanup();
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
