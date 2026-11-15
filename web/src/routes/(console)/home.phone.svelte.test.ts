import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte';
import { tick } from 'svelte';
import { page } from '$app/state';

const h = vi.hoisted(() => ({ needs: vi.fn(), agenda: vi.fn(), snooze: vi.fn(async () => ({})), load: vi.fn(async () => {}), invitations: vi.fn(async () => {}), listMyInvitations: vi.fn(), acceptMyInvitation: vi.fn(), tabsOpen: vi.fn(async () => {}), reset: () => {} }));
vi.mock('$lib/api/client', () => ({ api: { needs: { list: h.needs, snooze: h.snooze }, getMyAgenda: h.agenda, members: { listMyInvitations: h.listMyInvitations, acceptMyInvitation: h.acceptMyInvitation } } }));
vi.mock('$app/state', () => import('../../test/mocks/app-state-reactive.svelte'));
vi.mock('$app/navigation', () => ({ goto: vi.fn(), afterNavigate: vi.fn() }));
vi.mock('$lib/stores/breakpoint.svelte', () => ({ MOBILE_MEDIA_QUERY: '(max-width: 768px)', viewport: { isMobile: true } }));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { authenticated: true, userId: 'u', user: { name: 'Mara', timezone: 'UTC' }, identityFence: () => () => true, onIdentityChange: (reset: () => void) => { h.reset = reset; return () => {}; } } }));
vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: { current: null, workspaces: [], ensureAll: h.load, loadAll: h.load } }));
vi.mock('$lib/stores/tabs.svelte', () => ({ tabsStore: { open: h.tabsOpen } }));
vi.mock('$lib/stores/chatSetting.svelte', () => ({ chatSetting: { enabled: false } }));
vi.mock('$lib/stores/chatSurface.svelte', () => ({ chatSurface: { isOpen: false, closeThen: () => false } }));
vi.mock('$lib/components/layout/QuickCaptureSheet.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/components/layout/WorkspaceSheet.svelte', () => import('../../test/StubComponent.svelte'));

import Home from './+page.svelte';
import BottomNav from '$lib/components/layout/BottomNav.svelte';
import { homeOverview } from '$lib/stores/homeOverview.svelte';
import DesktopHome from '$lib/components/home/DesktopHome.svelte';
import { pendingInvitations } from '$lib/stores/pendingInvitations.svelte';

const invitation = {
	id: 'invite-1', role: 'editor', workspace_slug: 'invited', workspace_name: 'Invited Workspace',
	workspace_owner_username: 'alice', invited_by_name: 'Alice', created_at: '2026-11-14T00:00:00Z',
};

beforeEach(() => {
	vi.clearAllMocks();
	h.reset();
	pendingInvitations.set([invitation], pendingInvitations.reserve());
	vi.spyOn(pendingInvitations, 'refresh').mockImplementation(h.invitations);
	h.listMyInvitations.mockReset().mockResolvedValue({ invitations: [invitation], email_verified: true });
	h.acceptMyInvitation.mockReset().mockResolvedValue({ accepted: true });
	vi.useFakeTimers({ toFake: ['Date'] });
	vi.setSystemTime(new Date('2026-11-13T14:00:00Z'));
	Object.defineProperty(window, 'innerWidth', { value: 390, configurable: true });
	page.url = new URL('http://localhost/');
	h.needs.mockResolvedValue({ rows: Array.from({ length: 4 }, (_, i) => ({ workspace: { slug: 'ws', name: 'Northwind' }, item: { ref: `TASK-${i}`, title: `Needs row ${i}`, collection: 'tasks' }, why: { text: 'Waiting for you' }, because: [], waiting_since: '2026-11-12T10:00:00Z' })), total: 7, hidden: 0, workspaces: [{ slug: 'ws', count: 7 }] });
	h.agenda.mockResolvedValue({ entries: [], priority_list: [], window_complete: true });
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.useRealTimers(); });

it('expands the Home invitation count into actions and collapses after accepting', async () => {
	render(DesktopHome);
	const workspaces = screen.getByRole('region', { name: 'Workspaces' });
	const header = within(workspaces.querySelector('header')!);
	const toggle = header.getByRole('button', { name: '1 invitation', expanded: false });
	expect(toggle).toHaveAttribute('type', 'button');
	expect(toggle).toHaveAttribute('aria-controls', 'home-invitations');
	await fireEvent.click(toggle);
	expect(toggle).toHaveAttribute('aria-expanded', 'true');
	const accept = within(document.getElementById('home-invitations')!).getByRole('button', { name: 'Accept the invitation to Invited Workspace' });
	expect(h.listMyInvitations).toHaveBeenCalledTimes(1);
	// Leave another invitation so collapse must come from onaccepted, not an empty list.
	pendingInvitations.set([invitation, { ...invitation, id: 'invite-2', workspace_name: 'Another Workspace' }], pendingInvitations.reserve());
	await tick();
	await fireEvent.click(accept);
	await vi.waitFor(() => expect(h.acceptMyInvitation).toHaveBeenCalledWith('invite-1'));
	expect(header.getByRole('button', { name: '1 invitation' })).toHaveAttribute('aria-expanded', 'false');
	expect(document.getElementById('home-invitations')).not.toBeInTheDocument();
});

it('returns zero Home invitations to plain text without a toggle or list', async () => {
	render(DesktopHome);
	const workspaces = within(screen.getByRole('region', { name: 'Workspaces' }));
	await fireEvent.click(workspaces.getByRole('button', { name: '1 invitation' }));
	pendingInvitations.remove(invitation.id);
	await tick();
	expect(workspaces.getByText('0 · 0 invitations')).toBeInTheDocument();
	expect(workspaces.queryByRole('button', { name: /invitations/i })).not.toBeInTheDocument();
	expect(workspaces.queryByRole('region', { name: 'Invitations' })).not.toBeInTheDocument();
});

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

it('shows today’s completed expander on phone and expands compact rows', async () => {
	h.agenda.mockResolvedValue({ entries: [{ type: 'completed', at: '2026-11-13T12:00:00Z', day: '2026-11-13', item_ref: 'TASK-90', item_title: 'Finished on phone', collection: 'tasks', workspace: 'ws', owner_username: 'mara' }], priority_list: [], window_complete: true });
	render(BottomNav);
	render(Home);
	const today = within(screen.getByRole('region', { name: 'Today' }));
	const toggle = await today.findByRole('button', { name: '1 completed', expanded: false });
	expect(today.queryByText('Finished on phone')).not.toBeInTheDocument();
	await fireEvent.click(toggle);
	expect(today.getByText('Finished on phone').closest('.agenda-row')).toHaveClass('compact');
	expect(today.getByText('Nothing scheduled today.')).toBeInTheDocument();
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
