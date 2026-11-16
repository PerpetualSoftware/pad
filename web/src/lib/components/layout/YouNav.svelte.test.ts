import { beforeEach, afterEach, it, expect, vi } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';

vi.mock('$app/state', async () => await import('./youNavPage.fixture.svelte'));
vi.mock('$lib/api/client', () => ({
	api: {
		needs: { list: vi.fn(async () => ({ rows: [], total: 2, hidden: 0, workspaces: [] })) },
		getMyAgenda: vi.fn(async () => ({ entries: [], priority_list: [] }))
	}
}));
vi.mock('$lib/stores/chatSetting.svelte', () => ({ chatSetting: { enabled: false } }));
const h = vi.hoisted(() => ({ resets: [] as (() => void)[] }));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: {
 authenticated: true, userId: 'user-a', user: { timezone: 'UTC' },
 identityFence: () => () => true, onIdentityChange: (reset: () => void) => { h.resets.push(reset); return () => {}; }
} }));

import YouNav from './YouNav.svelte';
import { page } from '$app/state';
import { api } from '$lib/api/client';

async function go(path: string) { page.url = new URL('http://localhost' + path); for (let i = 0; i < 5; i++) await tick(); }

// TASK-4545: counts refresh on each arrival at a You page, and not while the
// user moves around inside a workspace.
it('refetches on returning to Home from a workspace, not on workspace navigation', async () => {
	render(YouNav);
	await go('/');
	const calls = () => vi.mocked(api.getMyAgenda).mock.calls.length;
	const start = calls();
	expect(start).toBeGreaterThan(0);
	await go('/u/ws/tasks');
	await go('/u/ws/ideas');
	expect(calls()).toBe(start);
	await go('/');
	expect(calls()).toBe(start + 1);
	expect(vi.mocked(api.needs.list).mock.calls.length).toBe(start + 1);
});


beforeEach(() => { h.resets.forEach(reset => reset()); vi.clearAllMocks(); page.url = new URL('http://localhost/'); });
afterEach(cleanup);
it('leaves agenda arrival to the page, then refreshes once on each needs arrival', async () => {
 page.url = new URL('http://localhost/agenda');
 render(YouNav); await go('/agenda');
 expect(api.getMyAgenda).not.toHaveBeenCalled();
 expect(api.needs.list).not.toHaveBeenCalled();
 await go('/needs');
 expect(api.getMyAgenda).toHaveBeenCalledTimes(1);
 expect(api.needs.list).toHaveBeenCalledTimes(1);
 await go('/agenda');
 expect(api.getMyAgenda).toHaveBeenCalledTimes(1);
 expect(api.needs.list).toHaveBeenCalledTimes(1);
 await go('/needs');
 expect(api.getMyAgenda).toHaveBeenCalledTimes(2);
 expect(api.needs.list).toHaveBeenCalledTimes(2);
});
