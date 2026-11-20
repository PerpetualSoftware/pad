import { afterEach, it, expect, vi } from 'vitest';
import { render, screen, cleanup, fireEvent, within } from '@testing-library/svelte';
import type { AgendaEntry } from '$lib/types';
import { tick } from 'svelte';
const h = vi.hoisted(() => ({ needs: vi.fn(), snooze: vi.fn(async () => ({})), agenda: vi.fn(), list: vi.fn(), getCommitment: vi.fn(), resolve: vi.fn(async () => ({})) }));
vi.mock('$lib/api/client', () => ({ api: { needs: { list: h.needs, snooze: h.snooze }, getMyAgenda: h.agenda, getCommitment: h.getCommitment, resolveCommitment: h.resolve, workspaces: { list: h.list } } }));
vi.mock('$app/state', async () => await import('$lib/components/layout/youNavPage.fixture.svelte'));
vi.mock('$lib/stores/chatSetting.svelte', () => ({ chatSetting: { enabled: false } }));
vi.mock('$app/navigation', () => ({ goto: vi.fn(), afterNavigate: vi.fn() }));
vi.mock('$lib/stores/pendingInvitations.svelte', () => ({ pendingInvitations: { count: 1, refresh: vi.fn() } }));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { authenticated: true, userId: 'u', user: { name: 'Mara Lindqvist', timezone: 'UTC' }, identityFence: () => () => true, onIdentityChange: () => () => {} } }));
import Home from './+page.svelte';
import YouNav from '$lib/components/layout/YouNav.svelte';
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); vi.useRealTimers(); localStorage.clear(); });
it('shares one overview read on desktop Home arrival with YouNav mounted (TASK-4559)', async () => {
	vi.useFakeTimers({ toFake: ['Date'] }); vi.setSystemTime(new Date('2026-11-13T14:00:00Z'));
	h.list.mockResolvedValue(Array.from({length: 5}, (_, i) => ({ id: String(i), slug: 'ws'+i, name: 'Workspace '+i, owner_username: 'mara', owner_id: 'u' })));
	const rows = Array.from({length: 4}, (_, i) => ({workspace: {slug:'ws0',name:'Northwind'},item:{ref:'TASK-'+i,title:'Needs row '+i,collection:'tasks'},why:{text:'Waiting for you'},because:[],waiting_since:'2026-11-12T10:00:00Z'}));
	h.needs.mockResolvedValue({ rows, total: 7, hidden: 0, workspaces: [{ slug: 'ws0', count: 7 }] });
	const entry = (i: number, day = '2026-11-13') => ({ type: 'schedule', state: 'outstanding', at: day+'T10:00:00Z', day, item_ref: 'TASK-'+i, item_title: 'Task '+i, collection: 'tasks', workspace: 'ws0', owner_username: 'mara', schedule_id: String(i), roles: { target: null, deadline: null } });
	h.agenda.mockResolvedValue({ entries: [{...entry(5, '2026-11-10'), state: 'missed'},entry(1),entry(2),entry(3),{...entry(4, '2026-11-11'), overdue: true}], priority_list: [], window_complete: true });
	render(Home);
	render(YouNav);
	expect(await screen.findByRole('heading', { name: 'Good afternoon, Mara' })).toBeTruthy();
	expect(await screen.findByText(/7 need you/)).toBeTruthy();
	expect(document.querySelector('.summary')).toHaveTextContent('7 need you · 3 owed today · 1 past deadline');
	expect(screen.getByRole('link', { name: 'Open the full agenda →' })).toHaveAttribute('href','/agenda');
	expect(screen.getAllByRole('link', { name: /Workspace \d/ })).toHaveLength(5);
	expect(document.querySelectorAll('[data-strip-day]')).toHaveLength(7);
	expect(screen.getByRole('group', { name: 'Audience' })).toBeTruthy();
	expect(document.querySelector('.today-panel .agenda-row')?.getAttribute('data-ref')).toBe('TASK-4');
	expect(h.agenda).toHaveBeenCalledWith(expect.any(String), expect.any(String), 'UTC', undefined, ['completed'], 'omit');
	expect(h.needs).toHaveBeenCalledTimes(1); expect(h.agenda).toHaveBeenCalledTimes(1); expect(h.list).toHaveBeenCalledTimes(1);
	expect(screen.getAllByRole('button', { name: 'Dismiss' })).toHaveLength(4);
	expect(screen.getAllByRole('button', { name: 'Defer' })).toHaveLength(4);
	h.needs.mockResolvedValueOnce({ rows: rows.slice(1), total: 6, hidden: 1, workspaces: [{slug: 'ws0', count: 6}] });
	await fireEvent.click(screen.getAllByRole('button', { name: 'Dismiss' })[0]);
	expect(h.snooze).toHaveBeenCalledWith('ws0', 'TASK-0', { kind: 'dismiss' });
	expect(await screen.findByRole('link', { name: 'See all 6 →' })).toBeTruthy();
	expect(h.list).toHaveBeenCalledTimes(1);
});

const completed = (item_ref = 'TASK-90', day = '2026-11-13'): AgendaEntry => ({
	type: 'completed', at: `${day}T12:00:00Z`, day, item_ref, item_title: `Finished ${item_ref}`,
	collection: 'tasks', workspace: 'ws0', owner_username: 'mara',
});

async function renderCompleted(entries: AgendaEntry[], events_truncated = false) {
	vi.useFakeTimers({ toFake: ['Date'] }); vi.setSystemTime(new Date('2026-11-13T14:00:00Z'));
	h.list.mockResolvedValue([]);
	h.needs.mockResolvedValue({ rows: [], total: 0, hidden: 0, workspaces: [] });
	h.agenda.mockResolvedValue({ entries, priority_list: [], window_complete: true, events_truncated });
	const { homeOverview } = await import('$lib/stores/homeOverview.svelte');
	await homeOverview.load('/', true);
	render(Home);
	await tick();
	return within(screen.getByRole('region', { name: 'Today' }));
}

it('folds only today’s completed entries below the empty Today rows', async () => {
	const today = await renderCompleted([completed(), completed('TASK-91', '2026-11-12')]);
	const toggle = today.getByRole('button', { name: '1 completed', expanded: false });
	expect(today.getByText('Nothing scheduled today.')).toBeInTheDocument();
	expect(today.queryByText('Finished TASK-90')).not.toBeInTheDocument();
	expect(toggle.closest('li')?.parentElement?.tagName).toBe('UL');
	await fireEvent.click(toggle);
	expect(toggle).toHaveAttribute('aria-expanded', 'true');
	expect(today.getByText('Finished TASK-90')).toBeInTheDocument();
	expect(today.queryByText('Finished TASK-91')).not.toBeInTheDocument();
	expect(today.getByText('Nothing scheduled today.')).toBeInTheDocument();
});

it('keeps completed and created events out of existing Home rows, figures, audience counts and week strip', async () => {
	const schedule = (item_ref: string, extra: Partial<AgendaEntry> = {}): AgendaEntry => ({
		...completed(item_ref), type: 'schedule', state: 'outstanding', schedule_id: item_ref, ...extra,
	});
	const today = await renderCompleted([
		schedule('TASK-1'), schedule('TASK-2', { audience: 'agent' }),
		schedule('TASK-3', { day: '2026-11-12', at: '2026-11-12T12:00:00Z', overdue: true }),
		completed(), { ...completed('TASK-91'), audience: 'agent' },
		completed('TASK-92', '2026-11-12'), { ...completed('TASK-93'), type: 'created' },
	]);
	expect(document.querySelector('.summary')).toHaveTextContent('0 need you · 2 owed today · 1 past deadline');
	const refs = () => [...screen.getByRole('region', { name: 'Today' }).querySelectorAll('.agenda-row')].map(row => row.getAttribute('data-ref'));
	expect(refs()).toEqual(['TASK-3', 'TASK-1']);
	for (const name of ['Mine 1', 'Agents 1', 'All 2']) expect(today.getByRole('button', { name })).toBeInTheDocument();
	expect(today.getByText('Agents: 1 reminders today, 1 owed, 0 missed')).toBeInTheDocument();
	expect(document.querySelector('[data-strip-day="2026-11-13"]')).toHaveAttribute('aria-label', 'Friday 13 November, Today, 2 entries, 2 owed');
	expect(document.querySelector('[data-strip-day="2026-11-12"]')).toHaveAttribute('aria-label', 'Thursday 12 November, 1 entry, 1 owed');
	expect(document.querySelectorAll('.pip')).toHaveLength(3);
	await fireEvent.click(today.getByRole('button', { name: 'All 2' }));
	expect(refs()).toEqual(['TASK-3', 'TASK-1', 'TASK-2']);
	await fireEvent.click(today.getByRole('button', { name: '2 completed' }));
	expect(today.queryByText('Finished TASK-93')).not.toBeInTheDocument();
	expect(document.querySelector('.summary')).toHaveTextContent('0 need you · 2 owed today · 1 past deadline');
});

it('counts a lapsed occurrence today as owed, since a late done is still accepted, and a missed one as missed', async () => {
	const schedule = (item_ref: string, extra: Partial<AgendaEntry> = {}): AgendaEntry => ({
		...completed(item_ref), type: 'schedule', state: 'outstanding', schedule_id: item_ref, ...extra,
	});
	const today = await renderCompleted([
		schedule('TASK-1', { state: 'lapsed' }),
		schedule('TASK-2', { state: 'lapsed', audience: 'agent' }), schedule('TASK-3', { state: 'missed', audience: 'agent' }),
	]);
	expect(document.querySelector('.summary')).toHaveTextContent(/ 2 owed today /);
	expect(today.getByText('Agents: 2 reminders today, 1 owed, 1 missed')).toBeInTheDocument();
	expect(document.querySelector('[data-strip-day="2026-11-13"]')).toHaveAttribute('aria-label', 'Friday 13 November, Today, 3 entries, 2 owed');
});

it('matches the agenda audience filter: even agent completions appear in Mine and All, never Agents', async () => {
	const today = await renderCompleted([completed(), { ...completed('TASK-91'), audience: 'agent' }]);
	await fireEvent.click(today.getByRole('button', { name: '2 completed' }));
	for (const ref of ['TASK-90', 'TASK-91']) expect(today.getByText(`Finished ${ref}`)).toBeInTheDocument();
	await fireEvent.click(today.getByRole('button', { name: 'Agents 0' }));
	expect(today.queryByRole('button', { name: /completed/ })).not.toBeInTheDocument();
	for (const ref of ['TASK-90', 'TASK-91']) expect(today.queryByText(`Finished ${ref}`)).not.toBeInTheDocument();
	await fireEvent.click(today.getByRole('button', { name: 'All 0' }));
	expect(today.getByRole('button', { name: '2 completed' })).toBeInTheDocument();
	for (const ref of ['TASK-90', 'TASK-91']) expect(today.getByText(`Finished ${ref}`)).toBeInTheDocument();
});

it.each(['0', '1', 'other', null])('reads the user’s completed preference once on mount (%s)', async value => {
	localStorage.setItem('pad-agenda-completed:other-user', '0');
	if (value !== null) localStorage.setItem('pad-agenda-completed:u', value);
	const storage = localStorage;
	const read = vi.fn((key: string) => storage.getItem(key));
	vi.stubGlobal('localStorage', { getItem: read });
	const today = await renderCompleted([completed()]);
	expect(read.mock.calls.filter(([key]) => key === 'pad-agenda-completed:u')).toHaveLength(1);
	if (value === '0') expect(today.queryByRole('button', { name: /completed/ })).not.toBeInTheDocument();
	else expect(today.getByRole('button', { name: '1 completed' })).toBeInTheDocument();
	await fireEvent.click(today.getByRole('button', { name: 'All 0' }));
	expect(read.mock.calls.filter(([key]) => key === 'pad-agenda-completed:u')).toHaveLength(1);
});

it('still shows completed entries when optional storage is unavailable', async () => {
	vi.stubGlobal('localStorage', { getItem: () => { throw new Error('Storage unavailable'); } });
	const today = await renderCompleted([completed()]);
	expect(today.getByRole('button', { name: '1 completed', expanded: false })).toBeInTheDocument();
});

it('shows the completed truncation note and agenda link inside Today even without completed rows', async () => {
	const today = await renderCompleted([], true);
	const note = today.getByText('Some completed items today may not be shown.', { exact: false });
	expect(note).toHaveClass('empty');
	expect(within(note).getByRole('link', { name: 'Open the full agenda →' })).toHaveAttribute('href', '/agenda');
});

it('hides the completed truncation note when the user hides completed work on the agenda', async () => {
	localStorage.setItem('pad-agenda-completed:u', '0');
	const today = await renderCompleted([completed()], true);
	expect(today.queryByText('Some completed items today may not be shown.', { exact: false })).not.toBeInTheDocument();
	expect(today.queryByRole('button', { name: /completed/ })).not.toBeInTheDocument();
});

// TASK-4545 review: Home runs the agenda page's occurrence guard, so Done never
// resolves a different occurrence that shares the clicked one's wall time.
it('refuses Done when the outstanding occurrence is a different instant at the same wall time', async () => {
	vi.useFakeTimers({ toFake: ['Date'] }); vi.setSystemTime(new Date('2026-11-13T14:00:00Z'));
	h.list.mockResolvedValue([]);
	h.needs.mockResolvedValue({ rows: [], total: 0, hidden: 0, workspaces: [] });
	h.agenda.mockResolvedValue({ entries: [{ type: 'schedule', state: 'outstanding', at: '2026-11-13T16:00:00Z', wall: '2026-11-13T16:00', occurrence_key: '2026-11-13T16:00:00Z|s1', day: '2026-11-13', item_ref: 'TASK-1', item_title: 'Pinned', collection: 'tasks', workspace: 'ws0', owner_username: 'mara', schedule_id: 's1', roles: { target: null, deadline: null } }], priority_list: [], window_complete: true });
	h.getCommitment.mockResolvedValue({ ended_at: null, next_occurrence: { at: '2026-11-13T17:00:00Z', wall: '2026-11-13T16:00' } });
	const { homeOverview } = await import('$lib/stores/homeOverview.svelte');
	await homeOverview.load('/', true);
	render(Home);
	await fireEvent.click(await screen.findByRole('button', { name: 'Done TASK-1' }));
	await vi.waitFor(() => expect(h.getCommitment).toHaveBeenCalled());
	expect(await screen.findByText(/Open the full agenda to review it/)).toBeTruthy();
	expect(h.resolve).not.toHaveBeenCalled();
});

it('hands a lapsed occurrence to the full agenda instead of resolving it (TASK-4545 review)', async () => {
	const { goto } = await import('$app/navigation');
	vi.useFakeTimers({ toFake: ['Date'] }); vi.setSystemTime(new Date('2026-11-13T14:00:00Z'));
	h.getCommitment.mockClear(); h.resolve.mockClear(); vi.mocked(goto).mockClear();
	h.list.mockResolvedValue([]);
	h.needs.mockResolvedValue({ rows: [], total: 0, hidden: 0, workspaces: [] });
	h.agenda.mockResolvedValue({ entries: [{ type: 'schedule', state: 'lapsed', at: '2026-11-13T09:00:00Z', wall: '2026-11-13T09:00', day: '2026-11-13', item_ref: 'TASK-2', item_title: 'Lapsed', collection: 'tasks', workspace: 'ws0', owner_username: 'mara', schedule_id: 's2', roles: { target: null, deadline: null } }], priority_list: [], window_complete: true });
	const { homeOverview } = await import('$lib/stores/homeOverview.svelte');
	await homeOverview.load('/', true);
	render(Home);
	await fireEvent.click(await screen.findByRole('button', { name: /TASK-2/ }));
	expect(goto).toHaveBeenCalledWith('/agenda');
	expect(h.getCommitment).not.toHaveBeenCalled();
	expect(h.resolve).not.toHaveBeenCalled();
});
it('opens the agenda on the tapped strip day (BUG-4569)', async () => {
	const { goto } = await import('$app/navigation');
	vi.useFakeTimers({ toFake: ['Date'] }); vi.setSystemTime(new Date('2026-11-13T14:00:00Z'));
	vi.mocked(goto).mockClear();
	h.list.mockResolvedValue([]);
	h.needs.mockResolvedValue({ rows: [], total: 0, hidden: 0, workspaces: [] });
	h.agenda.mockResolvedValue({ entries: [], priority_list: [], window_complete: true });
	render(Home);
	await fireEvent.click(document.querySelector<HTMLButtonElement>('[data-strip-day="2026-11-11"]')!);
	expect(goto).toHaveBeenCalledTimes(1);
	expect(goto).toHaveBeenCalledWith('/agenda?day=2026-11-11');
});
it('refetches the overview when the time zone changes on the same date (TASK-4595 review)', async () => {
	vi.useFakeTimers({ toFake: ['Date'] }); vi.setSystemTime(new Date('2026-11-13T14:00:00Z'));
	h.needs.mockResolvedValue({ rows: [], total: 0, hidden: 0, workspaces: [] });
	h.agenda.mockResolvedValue({ entries: [], priority_list: [], window_complete: true });
	const { authStore } = await import('$lib/stores/auth.svelte');
	const { homeOverview } = await import('$lib/stores/homeOverview.svelte');
	await homeOverview.load('/', true);
	h.agenda.mockClear();
	await homeOverview.load('/');
	expect(h.agenda).not.toHaveBeenCalled();
	const user = authStore.user as { timezone: string };
	user.timezone = 'Europe/Berlin';
	try {
		await homeOverview.load('/');
		expect(h.agenda).toHaveBeenCalledTimes(1);
		expect(h.agenda).toHaveBeenCalledWith(expect.any(String), expect.any(String), 'Europe/Berlin', undefined, ['completed'], 'omit');
	} finally { user.timezone = 'UTC'; }
});
