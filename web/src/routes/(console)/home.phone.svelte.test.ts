import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte';
import { tick } from 'svelte';
import { page } from '$app/state';

const h = vi.hoisted(() => ({ needs: vi.fn(), agenda: vi.fn(), snooze: vi.fn(async () => ({})), load: vi.fn(async () => {}), invitations: vi.fn(async () => {}), reset: () => {} }));
vi.mock('$lib/api/client', () => ({ api: { needs: { list: h.needs, snooze: h.snooze }, getMyAgenda: h.agenda } }));
vi.mock('$app/state', () => import('../../test/mocks/app-state-reactive.svelte'));
vi.mock('$app/navigation', () => ({ goto: vi.fn(), afterNavigate: vi.fn() }));
vi.mock('$lib/stores/breakpoint.svelte', () => ({ MOBILE_MEDIA_QUERY: '(max-width: 768px)', viewport: { isMobile: true } }));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { authenticated: true, userId: 'u', user: { name: 'Mara', timezone: 'UTC' }, identityFence: () => () => true, onIdentityChange: (reset: () => void) => { h.reset = reset; return () => {}; } } }));
vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: { current: null, workspaces: [], ensureAll: h.load } }));
vi.mock('$lib/stores/pendingInvitations.svelte', () => ({ pendingInvitations: { count: 1, refresh: h.invitations } }));
vi.mock('$lib/stores/chatSetting.svelte', () => ({ chatSetting: { enabled: false } }));
vi.mock('$lib/stores/chatSurface.svelte', () => ({ chatSurface: { isOpen: false, closeThen: () => false } }));
vi.mock('$lib/components/layout/QuickCaptureSheet.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/components/layout/WorkspaceSheet.svelte', () => import('../../test/StubComponent.svelte'));

import Home from './+page.svelte';
import BottomNav from '$lib/components/layout/BottomNav.svelte';
import { homeOverview } from '$lib/stores/homeOverview.svelte';
import DesktopHome from '$lib/components/home/DesktopHome.svelte';

beforeEach(() => {
	vi.clearAllMocks();
	h.reset();
	vi.useFakeTimers({ toFake: ['Date'] });
	vi.setSystemTime(new Date('2026-11-13T14:00:00Z'));
	Object.defineProperty(window, 'innerWidth', { value: 390, configurable: true });
	page.url = new URL('http://localhost/');
	h.needs.mockResolvedValue({ rows: Array.from({ length: 4 }, (_, i) => ({ workspace: { slug: 'ws', name: 'Northwind' }, item: { ref: `TASK-${i}`, title: `Needs row ${i}`, collection: 'tasks' }, why: { text: 'Waiting for you' }, because: [], waiting_since: '2026-11-12T10:00:00Z' })), total: 7, hidden: 0, workspaces: [{ slug: 'ws', count: 7 }] });
	h.agenda.mockResolvedValue({ entries: [], priority_list: [], window_complete: true });
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.useRealTimers(); });

it('phone / renders Home with two Needs you rows, week strip and Today instead of Your Workspaces', async () => {
	render(BottomNav);
	render(Home);
	expect(await screen.findByRole('heading', { name: 'Good afternoon, Mara' })).toBeInTheDocument();
	expect(await screen.findByText('5 more')).toBeInTheDocument();
	expect(document.querySelector('.summary')).toHaveTextContent('7 need you · 0 owed today · 0 past deadline');
	expect(within(screen.getByRole('region', { name: 'Needs you' })).getAllByTestId('needs-row')).toHaveLength(2);
	expect(screen.getByRole('link', { name: 'See all 7 →' })).toHaveAttribute('href', '/needs');
	expect(document.querySelectorAll('[data-strip-day]')).toHaveLength(7);
	const today = within(screen.getByRole('region', { name: 'Today' }));
	for (const name of ['Mine', 'Agents', 'All']) expect(today.getByRole('button', { name: new RegExp(name) })).toBeInTheDocument();
	expect(screen.queryByRole('heading', { name: 'Your Workspaces' })).not.toBeInTheDocument();
	expect(screen.getByText(/1 invitation/)).toBeInTheDocument();
	expect(h.invitations).toHaveBeenCalledTimes(1);
	await fireEvent.click(screen.getByRole('button', { name: 'Actions TASK-0' }));
	await fireEvent.click(screen.getByRole('button', { name: 'Dismiss', exact: true }));
	expect(h.snooze).toHaveBeenCalledWith('ws', 'TASK-0', { kind: 'dismiss' });
});

it.each(['home-first', 'bar-first'])('loads the overview once on phone arrival with shared Home and bar (%s)', async order => {
	const load = vi.spyOn(homeOverview, 'load');
	// Exercise the real shared component even before / is wired to it on phones.
	for (const component of order === 'home-first' ? [DesktopHome, BottomNav] : [BottomNav, DesktopHome]) render(component);
	await tick();
	await vi.waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(1));
	expect(h.needs).toHaveBeenCalledTimes(1);
	expect(load).toHaveBeenCalledTimes(1);
	load.mockRestore();
});

it('refreshes the phone overview once per You-page arrival', async () => {
	render(BottomNav);
	const home = render(Home);
	await vi.waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(1));
	await home.unmount();
	for (const [path, calls] of [['/needs', 2], ['/agenda', 3], ['/', 4]] as const) {
		page.url = new URL(`http://localhost${path}`);
		if (path === '/') render(Home);
		await tick();
		await vi.waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(calls));
		expect(h.needs).toHaveBeenCalledTimes(calls);
	}
});
