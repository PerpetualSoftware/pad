import { afterEach, it, expect, vi } from 'vitest';
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte';
const h = vi.hoisted(() => ({ needs: vi.fn(), snooze: vi.fn(async () => ({})), agenda: vi.fn(), list: vi.fn(), getCommitment: vi.fn(), resolve: vi.fn(async () => ({})) }));
vi.mock('$lib/api/client', () => ({ api: { needs: { list: h.needs, snooze: h.snooze }, getMyAgenda: h.agenda, getCommitment: h.getCommitment, resolveCommitment: h.resolve, workspaces: { list: h.list } } }));
vi.mock('$app/navigation', () => ({ goto: vi.fn(), afterNavigate: vi.fn() }));
vi.mock('$lib/stores/pendingInvitations.svelte', () => ({ pendingInvitations: { count: 1, refresh: vi.fn() } }));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { userId: 'u', user: { name: 'Mara Lindqvist', timezone: 'UTC' }, identityFence: () => () => true, onIdentityChange: () => () => {} } }));
import Home from './+page.svelte';
afterEach(() => { cleanup(); vi.useRealTimers(); });
it('renders Home counts with one call per data source, independent of workspace count', async () => {
	vi.useFakeTimers({ toFake: ['Date'] }); vi.setSystemTime(new Date('2026-11-13T14:00:00Z'));
	h.list.mockResolvedValue(Array.from({length: 5}, (_, i) => ({ id: String(i), slug: 'ws'+i, name: 'Workspace '+i, owner_username: 'mara', owner_id: 'u' })));
	const rows = Array.from({length: 4}, (_, i) => ({workspace: {slug:'ws0',name:'Northwind'},item:{ref:'TASK-'+i,title:'Needs row '+i,collection:'tasks'},why:{text:'Waiting for you'},because:[],waiting_since:'2026-11-12T10:00:00Z'}));
	h.needs.mockResolvedValue({ rows, total: 7, hidden: 0, workspaces: [{ slug: 'ws0', count: 7 }] });
	const entry = (i: number, day = '2026-11-13') => ({ type: 'schedule', state: 'outstanding', at: day+'T10:00:00Z', day, item_ref: 'TASK-'+i, item_title: 'Task '+i, collection: 'tasks', workspace: 'ws0', owner_username: 'mara', schedule_id: String(i), roles: { target: null, deadline: null } });
	h.agenda.mockResolvedValue({ entries: [{...entry(5, '2026-11-10'), state: 'missed'},entry(1),entry(2),entry(3),{...entry(4, '2026-11-11'), overdue: true}], priority_list: [], window_complete: true });
	render(Home);
	expect(await screen.findByRole('heading', { name: 'Good afternoon, Mara' })).toBeTruthy();
	expect(await screen.findByText(/7 need you/)).toBeTruthy();
	expect(document.querySelector('.summary')).toHaveTextContent('7 need you · 3 owed today · 1 past deadline');
	expect(screen.getByRole('link', { name: 'Open the full agenda →' })).toHaveAttribute('href','/agenda');
	expect(screen.getAllByRole('link', { name: /Workspace \d/ })).toHaveLength(5);
	expect(document.querySelectorAll('[data-strip-day]')).toHaveLength(7);
	expect(screen.getByRole('group', { name: 'Audience' })).toBeTruthy();
	expect(document.querySelector('.today-panel .agenda-row')?.getAttribute('data-ref')).toBe('TASK-4');
	expect(h.needs).toHaveBeenCalledTimes(1); expect(h.agenda).toHaveBeenCalledTimes(1); expect(h.list).toHaveBeenCalledTimes(1);
	expect(screen.getAllByRole('button', { name: 'Dismiss' })).toHaveLength(4);
	expect(screen.getAllByRole('button', { name: 'Defer' })).toHaveLength(4);
	h.needs.mockResolvedValueOnce({ rows: rows.slice(1), total: 6, hidden: 1, workspaces: [{slug: 'ws0', count: 6}] });
	await fireEvent.click(screen.getAllByRole('button', { name: 'Dismiss' })[0]);
	expect(h.snooze).toHaveBeenCalledWith('ws0', 'TASK-0', { kind: 'dismiss' });
	expect(await screen.findByRole('link', { name: 'See all 6 →' })).toBeTruthy();
	expect(h.list).toHaveBeenCalledTimes(1);
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
