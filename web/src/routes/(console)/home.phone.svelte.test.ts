import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte';
import { stripPointer, swipeStrip } from '../../test/weekStripGesture';
import type { AgendaEntry, AgendaResponse } from '$lib/types';
import { tick } from 'svelte';
import { page } from '$app/state';

const h = vi.hoisted(() => ({ needs: vi.fn(), agenda: vi.fn(), snooze: vi.fn(async () => ({})), load: vi.fn(async () => {}), invitations: vi.fn(async () => {}), listMyInvitations: vi.fn(), acceptMyInvitation: vi.fn(), tabsOpen: vi.fn(async () => {}), reset: () => {}, epoch: 0 }));
vi.mock('$lib/api/client', () => ({ api: { needs: { list: h.needs, snooze: h.snooze }, getMyAgenda: h.agenda, members: { listMyInvitations: h.listMyInvitations, acceptMyInvitation: h.acceptMyInvitation } } }));
vi.mock('$app/state', () => import('../../test/mocks/app-state-reactive.svelte'));
vi.mock('$app/navigation', () => ({ goto: vi.fn(), afterNavigate: vi.fn() }));
vi.mock('$lib/stores/breakpoint.svelte', () => ({ MOBILE_MEDIA_QUERY: '(max-width: 768px)', viewport: { isMobile: true } }));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { authenticated: true, userId: 'u', user: { name: 'Mara', timezone: 'UTC' }, identityFence: () => { const epoch = h.epoch; return () => epoch === h.epoch; }, onIdentityChange: (reset: () => void) => { h.reset = reset; return () => {}; } } }));
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
	vi.clearAllMocks(); h.epoch = 0;
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

it('refreshes the phone overview on Home and Needs arrivals, leaving Agenda to its page', async () => {
	render(BottomNav);
	const home = render(Home);
	await vi.waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(1));
	await home.unmount();
	for (const [path, calls] of [['/needs', 2], ['/agenda', 2], ['/', 3]] as const) {
		page.url = new URL(`http://localhost${path}`);
		if (path === '/') render(Home);
		await tick();
		await vi.waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(calls));
		expect(h.needs).toHaveBeenCalledTimes(calls);
	}
});

const weekEntry =	(day: string, extra: Partial<AgendaEntry> = {}): AgendaEntry => ({
	type: 'schedule', state: 'outstanding', day, at: `${day}T16:00:00Z`, item_ref: 'TASK-42', item_title: 'Today work', collection: 'tasks', workspace: 'ws', ...extra
});
const weekResponse = (entries: AgendaEntry[] = [], window_complete = true): AgendaResponse => ({
	from: '', to: '', tz: 'UTC', entries, window_complete, priority_list: [], priority_list_complete: true, waiting_on: [], waiting_on_complete: true, no_date: [], no_date_complete: true
});
const deferredWeek = () => {
	let resolve!: (value: AgendaResponse) => void;
	const promise = new Promise<AgendaResponse>(r => resolve = r);
	return { promise, resolve };
};
const firstStripDay = () => document.querySelector<HTMLButtonElement>('[data-strip-day]')!;
async function mountSwipeHome() {
	h.agenda.mockResolvedValueOnce(weekResponse([weekEntry('2026-11-13')]));
	await homeOverview.load('/', true);
	render(Home);
	await tick();
}

it('swipes Home to next week, fetches only the strip and returns to This week without a request', async () => {
	await mountSwipeHome();
	const today = screen.getByRole('region', { name: 'Today' }).textContent;
	const needs = screen.getByRole('region', { name: 'Needs you' }).textContent;
	const summary = document.querySelector('.summary')!.textContent;
	expect(firstStripDay().dataset.stripDay).toBe('2026-11-09');
	expect(screen.queryByRole('button', { name: 'This week' })).not.toBeInTheDocument();
	h.agenda.mockResolvedValueOnce(weekResponse([weekEntry('2026-11-16'), weekEntry('2026-11-16', { type: 'completed' }), weekEntry('2026-11-16', { type: 'created' })]));
	await swipeStrip(screen.getByRole('group', { name: 'Days' }));
	expect(firstStripDay().dataset.stripDay).toBe('2026-11-16');
	expect(h.agenda).toHaveBeenCalledTimes(2);
	expect(h.agenda).toHaveBeenLastCalledWith('2026-11-16T00:00:00.000Z', '2026-11-23T00:00:00.000Z', 'UTC', undefined, ['completed'], 'omit');
	expect(firstStripDay()).toHaveAccessibleName(/1 entry, 1 owed/);
	expect(screen.getByText('16–22 Nov')).toBeInTheDocument();
	expect(screen.getByRole('region', { name: 'Today' }).textContent).toBe(today);
	expect(screen.getByRole('region', { name: 'Needs you' }).textContent).toBe(needs);
	expect(document.querySelector('.summary')!.textContent).toBe(summary);
	await fireEvent.click(screen.getByRole('button', { name: 'This week' }));
	expect(firstStripDay().dataset.stripDay).toBe('2026-11-09');
	expect(h.agenda).toHaveBeenCalledTimes(2);
	expect(screen.queryByRole('button', { name: 'This week' })).not.toBeInTheDocument();
	await vi.waitFor(() => expect(document.activeElement).toHaveAttribute('data-strip-day'));
	expect(document.activeElement).toHaveAttribute('tabindex', '0');
	const { goto } = await import('$app/navigation');
	await stripPointer(firstStripDay(), 'pointerdown', 200);
	await stripPointer(firstStripDay(), 'pointerup', 200);
	await fireEvent.click(firstStripDay()); expect(goto).toHaveBeenCalledWith('/agenda');
});

it('keeps the second swipe response when the first response arrives late', async () => {
	await mountSwipeHome();
	const first = deferredWeek(); const second = deferredWeek();
	h.agenda.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise);
	const root = screen.getByRole('group', { name: 'Days' });
	await swipeStrip(root);
	expect(firstStripDay()).toHaveAttribute('data-load', 'none');
	await swipeStrip(root);
	expect(firstStripDay().dataset.stripDay).toBe('2026-11-23');
	expect(h.agenda).toHaveBeenCalledTimes(3);
	second.resolve(weekResponse([weekEntry('2026-11-23')], false)); await tick();
	expect(firstStripDay()).toHaveAttribute('data-load', 'partial');
	expect(firstStripDay().querySelectorAll('.pip')).toHaveLength(1);
	first.resolve(weekResponse([weekEntry('2026-11-16')])); await tick();
	expect(firstStripDay()).toHaveAttribute('data-load', 'partial');
	expect(firstStripDay().querySelectorAll('.pip')).toHaveLength(1);
});

it('shows no strip counts or banner on failure and ignores a response from an old identity', async () => {
	await mountSwipeHome();
	h.agenda.mockRejectedValueOnce(new Error('offline'));
	await swipeStrip(screen.getByRole('group', { name: 'Days' }));
	expect(firstStripDay()).toHaveAttribute('data-load', 'none');
	expect(screen.queryByRole('alert')).not.toBeInTheDocument();
	const pending = deferredWeek(); h.agenda.mockReturnValueOnce(pending.promise);
	await swipeStrip(screen.getByRole('group', { name: 'Days' }));
	h.epoch++;
	pending.resolve(weekResponse([weekEntry('2026-11-23')])); await tick();
	expect(firstStripDay()).toHaveAttribute('data-load', 'none');
	expect(firstStripDay().querySelectorAll('.pip')).toHaveLength(0);
});
