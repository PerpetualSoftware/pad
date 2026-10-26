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
vi.mock('$lib/stores/chat.svelte', () => ({ chatStore: { unreadTotal: 4 } }));

import YouSheet from './YouSheet.svelte';

beforeEach(() => cleanup());
afterEach(() => cleanup());

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
