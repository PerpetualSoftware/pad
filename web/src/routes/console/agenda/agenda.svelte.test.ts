import { beforeEach, afterEach, describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent, waitFor, cleanup, within } from '@testing-library/svelte';
import { flushSync } from 'svelte';
import type { AgendaEntry, AgendaResponse, AgendaWorkEntry, CommitmentMoveResult, WorkspaceGroup, WorkspaceGroupsResponse } from '$lib/types';

const h = vi.hoisted(() => ({ toast: vi.fn(), agenda: vi.fn(), groups: vi.fn(), workspaces: vi.fn(), createGroup: vi.fn(), updateGroup: vi.fn(), removeGroup: vi.fn(), view: vi.fn(), resolve: vi.fn(), setForecast: vi.fn(), clearForecast: vi.fn(), preview: vi.fn(), move: vi.fn(), item: vi.fn(), update: vi.fn(), dismiss: vi.fn(), user: { id: 'user-a', timezone: 'America/New_York' } as { id: string; timezone?: string } | null, epoch: 0, signal: null as null | { set(key: string, value: number): unknown }, browserZone: 'Europe/London' as string | null }));
vi.mock('$lib/api/client', () => ({ api: { getMyAgenda: h.agenda, workspaceGroups: { list: h.groups, create: h.createGroup, update: h.updateGroup, remove: h.removeGroup }, workspaces: { list: h.workspaces }, getCommitment: h.view, setCommitmentForecast: h.setForecast, clearCommitmentForecast: h.clearForecast, resolveCommitment: h.resolve, previewCommitmentMove: h.preview, moveCommitment: h.move, dismissEndedCommitment: h.dismiss, items: { get: h.item, update: h.update } } }));
vi.mock('$lib/timezone', () => ({ browserZone: () => h.browserZone }));
vi.mock('$lib/stores/toast.svelte', () => ({ toastStore: { show: h.toast } }));
vi.mock('$lib/stores/auth.svelte', async () => {
 const { SvelteMap } = await import('svelte/reactivity');
 const signal = new SvelteMap<string, number>(); h.signal = signal;
 return { authStore: {
  get user() { signal.get('auth'); return h.user; },
  get identityEpoch() { signal.get('auth'); return h.epoch; },
  identityFence() { const epoch = h.epoch; return () => epoch === h.epoch; },
  onIdentityChange: () => () => {}
 } };
});
import Agenda from './+page.svelte';
import { confirmDialog } from '$lib/stores/confirmDialog.svelte';

const row = (extra: Partial<AgendaEntry> = {}): AgendaEntry => ({ type: 'schedule', at: '2026-11-05T14:00:00Z', day: '2026-11-05', wall: '2026-11-05T09:00:00', floating: true, item_ref: 'EX-30', item_title: 'First item', collection: 'tasks', workspace: 'work', workspace_name: 'First workspace', owner_username: 'alice', state: 'outstanding', roles: { target: null, earliest: null, deadline: null, deadline_behavior: null, occupied_until: null, outcome: '', roles_origin: 'native' }, schedule_id: 'schedule-1', ...extra });
const workspaceGroups: WorkspaceGroup[] = [{ id: 'all', name: 'All', kind: 'all', workspace_ids: [] }, { id: 'mine', name: 'Mine', kind: 'mine', workspace_ids: [] }, { id: 'group-1', name: 'Team', kind: 'named', workspace_ids: ['ws-1'] }];
const workspace = { id: 'ws-1', name: 'First workspace', slug: 'work', owner_username: 'alice' };
const frozenAt = '2026-11-05T17:00:00Z';
const response = (entries: AgendaEntry[] = [row()], extra: Partial<AgendaResponse> = {}): AgendaResponse => ({ from: '', to: '', tz: 'America/New_York', entries, window_complete:true,priority_list:[],priority_list_complete:true,waiting_on:[],waiting_on_complete:true,no_date:[],no_date_complete:true, ...extra });
const deferred = <T,>() => { let resolve!: (value: T) => void; const promise = new Promise<T>(r => { resolve = r; }); return { promise, resolve }; };
const settle = async () => { await new Promise(r => setTimeout(r, 0)); flushSync(); };
async function mount() { render(Agenda); await screen.findAllByText('First item'); }
async function menu(ref = 'EX-30') { await fireEvent.click(screen.getByRole('button', { name: `Actions ${ref}` })); }
async function dialog(kind: 'Move' | 'Snooze') {
 await menu();
 await fireEvent.click(screen.getByRole('menuitem', { name: kind === 'Move' ? 'Move target…' : 'Snooze…' }));
 return screen.getByRole('dialog', { name: kind === 'Move' ? 'Move target' : 'Snooze' });
}
/** A message that went to the toast container, with its type. */
const said = (text: string, type: 'success' | 'error' = 'error') => waitFor(() => expect(h.toast.mock.calls.filter(call => call[0] === text && call[1] === type)).toHaveLength(1));
const staleCopy = 'This occurrence changed. Review the updated entry and try again.';
async function completedOption() { await fireEvent.click(screen.getByRole('button', { name: 'View options' })); return screen.getByRole('menuitemradio', { name: 'Show completed' }); }
async function toggleCompleted() { await fireEvent.click(await completedOption()); }
async function completedShown() { const on = (await completedOption()).getAttribute('aria-checked') === 'true'; await fireEvent.click(screen.getByRole('button', { name: 'View options' })); return on; }
const filter = () => screen.getByRole('button', { name: /^Workspaces: / });
const groupItems = () => screen.getAllByRole('menuitemradio').map(el => el.querySelector('.mi-label')?.textContent?.trim());
const groupItem = (name: string) => screen.getAllByRole('menuitemradio').find(el => el.querySelector('.mi-label')?.textContent?.trim() === name)!;
const sections = () => [...document.querySelectorAll<HTMLElement>('section[data-day]')];
const stripDay = (day = '2026-11-06') => document.querySelector(`button[data-strip-day="${day}"]`) as HTMLButtonElement;
const stripDays = () => [...document.querySelectorAll<HTMLButtonElement>('button[data-strip-day]')];
async function fillTime(d: HTMLElement, value = '2026-11-06T09:30') { await fireEvent.input(within(d).getByLabelText(/New date and time|Remind me again at/), { target: { value } }); }

const preview = (to = '2026-11-06T14:00:00.000Z', extra: Partial<CommitmentMoveResult> = {}): CommitmentMoveResult => ({ revision: 'revision-1', changes: [{ item_id: 'root', role: 'target', from: row().at, to, root: true }], ...extra });
const workRow = (extra: Partial<AgendaWorkEntry> = {}): AgendaWorkEntry => ({ ...row(), type: 'schedule', rank: 'normal', reason: 'outstanding', ...extra });
const card = (ref = 'EX-30') => document.querySelector(`[data-ref="${ref}"][draggable="true"]`) as HTMLElement;
const daySection = (day = '2026-11-06') => document.querySelector(`section[data-day="${day}"]`) as HTMLElement;
async function drag(day = '2026-11-06', ref = 'EX-30') {
 await fireEvent.dragStart(card(ref)); await fireEvent.dragEnter(daySection(day));
}
async function drop(day = '2026-11-06', ref = 'EX-30') { await drag(day, ref); await fireEvent.drop(daySection(day)); }

beforeEach(() => {
 vi.resetAllMocks(); localStorage.clear(); h.user = { id: 'user-a', timezone: 'America/New_York' }; h.epoch = 0; h.browserZone = 'Europe/London'; h.signal?.set('auth', 0);
 vi.spyOn(Date, 'now').mockReturnValue(Date.parse('2026-11-05T12:00:00Z'));
 h.agenda.mockResolvedValue(response());
 h.groups.mockResolvedValue({ groups: workspaceGroups }); h.workspaces.mockResolvedValue([workspace]);
 h.createGroup.mockResolvedValue({}); h.updateGroup.mockResolvedValue({}); h.removeGroup.mockResolvedValue({});
 h.view.mockResolvedValue({ item_ref: 'EX-30', roles: { target: null, earliest: null, deadline: null, deadline_behavior: null, occupied_until: null, outcome: '', roles_origin: 'native' }, floating: true, next_occurrence: { at: frozenAt, wall: row().wall } });
 h.preview.mockImplementation((_ws, _ref, body) => Promise.resolve(preview(body.to)));
 h.move.mockResolvedValue(preview()); h.item.mockResolvedValue({ title: 'Linked child' });
 h.resolve.mockResolvedValue({ schedule: {}, resolution: { occurrence_at: frozenAt, state: 'done', missed_recorded: 0 } });
});
afterEach(() => { confirmDialog.abandonAll(); cleanup(); vi.restoreAllMocks(); });

describe('agenda Day page', () => {
 beforeEach(() => localStorage.setItem('pad-agenda-mode:user-a', 'day'));
 it('renders two workspaces, routes, time, checkbox and one polite status', async () => {
  h.agenda.mockResolvedValue(response([row(), row({ workspace: 'other', owner_username: 'bob', workspace_name: 'Second workspace', item_ref: 'EX-31', item_title: 'Second item', schedule_id: 'schedule-2', all_day: true })]));
  await mount();
  expect(screen.getByText('First workspace')).toBeTruthy(); expect(screen.getByText('Second workspace')).toBeTruthy();
  expect(screen.getByRole('link', { name: 'First item' }).getAttribute('href')).toBe('/alice/work/tasks/EX-30');
  expect(screen.getByText('09:00')).toBeTruthy(); expect(screen.getByText('All day')).toBeTruthy();
  expect(await completedShown()).toBe(true);
  expect(screen.getAllByRole('status')).toHaveLength(1);
  expect(screen.getByRole('link', { name: 'America/New_York' }).getAttribute('href')).toBe('/console/settings');
  expect(h.agenda.mock.calls[0][2]).toBe('America/New_York');
  expect(screen.getByRole('button', { name: 'Month' })).toHaveAttribute('aria-pressed', 'false');
 });
 it('hides only completed entries locally, persists per user, and another user defaults to checked', async () => {
  h.agenda.mockResolvedValue(response([row(), row({ type: 'completed', item_ref: 'EX-31', item_title: 'Completed item' }), row({ type: 'completed', item_ref: 'EX-32', item_title: 'Other completed' })]));
  const completed = () => screen.queryByRole('button', { name: '2 completed' });
  await mount(); expect(completed()).toBeTruthy(); expect(screen.queryByText('Completed item')).toBeNull();
  await fireEvent.click(completed()!); expect(screen.getByText('Completed item')).toBeTruthy(); expect(screen.getByText('Other completed')).toBeTruthy();
  await toggleCompleted();
  expect(completed()).toBeNull(); expect(screen.queryByText('Completed item')).toBeNull(); expect(screen.getByText('First item')).toBeTruthy();
  expect(localStorage.getItem('pad-agenda-completed:user-a')).toBe('0'); expect(h.agenda).toHaveBeenCalledTimes(1);
  cleanup(); await mount(); expect(completed()).toBeNull();
  cleanup(); h.user = { id: 'user-b', timezone: 'America/New_York' }; await mount(); expect(completed()).toBeTruthy();
 });
 it('refetches timezone changes on the mounted page and isolates a new user preference', async () => {
  await mount(); await toggleCompleted();
  h.user = { id: 'user-a', timezone: 'Asia/Tokyo' }; h.signal?.set('auth', 1);
  await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(2));
  expect(h.agenda.mock.calls[1]).toEqual(['2026-11-04T15:00:00.000Z', '2026-11-05T15:00:00.000Z', 'Asia/Tokyo', 'all', ['completed']]);
  h.user = { id: 'user-b', timezone: 'UTC' }; h.epoch++; h.signal?.set('auth', 2);
  await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(3));
  expect(await completedShown()).toBe(true);
  expect(localStorage.getItem('pad-agenda-completed:user-b')).toBeNull();
 });
 it.each(['Europe/London', null])('uses the browser zone, then UTC, when the account has no zone: %s', async browserZone => {
  h.user = { id: 'user-a' }; h.browserZone = browserZone;
  await mount(); expect(h.agenda.mock.calls[0][2]).toBe(browserZone ?? 'UTC');
 });
 it('renders history styling and moved time without creating links with missing route fields', async () => {
  h.agenda.mockResolvedValue(response([
   row({ state: 'done', item_title: 'Finished occurrence' }),
   row({ state: 'moved', item_ref: 'EX-31', schedule_id: 's2', item_title: 'Moved occurrence', at: '2026-11-06T15:30:00Z' }),
   row({ owner_username: '', item_ref: 'EX-32', schedule_id: 's3', item_title: 'No owner' })
  ]));
  render(Agenda); const done = await screen.findByRole('link', { name: 'Finished occurrence' });
  expect(done.closest('li')?.classList.contains('done')).toBe(true); expect(screen.getByRole('img', { name: 'Done' })).toBeTruthy();
  expect(screen.getByText('10:30')).toBeTruthy(); expect(screen.getByText('No owner')).toBeTruthy();
  expect(screen.queryByRole('link', { name: 'No owner' })).toBeNull();
 });
 it('survives throwing storage with an in-memory default', async () => {
  vi.spyOn(localStorage, 'getItem').mockImplementation(() => { throw new Error('blocked'); });
  vi.spyOn(localStorage, 'setItem').mockImplementation(() => { throw new Error('blocked'); });
  await mount(); expect(await completedShown()).toBe(true);
  await toggleCompleted(); expect(await completedShown()).toBe(false);
 });
 it('does not access the preference before the user is known', async () => {
  h.user = null; const get = vi.spyOn(localStorage, 'getItem'); const set = vi.spyOn(localStorage, 'setItem');
  render(Agenda); await settle(); expect(get).not.toHaveBeenCalled(); expect(set).not.toHaveBeenCalled(); expect(h.agenda).not.toHaveBeenCalled();
 });
 it.each([{ truncated: true, next_from: '2026-11-06T01:00:00Z' }, { unavailable: [{ item_ref: 'EX-30', code: 'internal' }] }, { unexpanded: [{ schedule_id: 's', item_ref: 'EX-30', item_title: 'X', collection: 'tasks', reason: 'scan_limit' as const }] }])('never claims an incomplete answer is empty: %j', async extra => {
  h.agenda.mockResolvedValue(response([], extra)); render(Agenda);
  await screen.findByText('next_from' in extra ? 'Showing entries up to Thu 5 November.' : 'Some schedules could not be shown.');
  expect(screen.queryByText(/Nothing scheduled/)).toBeNull();
  expect(screen.queryAllByRole('button', { name: 'Load the rest' })).toHaveLength('next_from' in extra ? 1 : 0);
 });
 it('says when completed items were cut, only while completed work is shown (BUG-4527)', async () => {
  h.agenda.mockResolvedValue(response([row({ type: 'completed' })], { events_truncated: true })); render(Agenda);
  expect(await screen.findByText(/Some completed items in this \w+ aren't shown/)).toBeTruthy();
  expect(screen.queryByText(/Too much to load/)).toBeNull();
  await toggleCompleted();
  expect(screen.queryByText(/Some completed items in this \w+ aren't shown/)).toBeNull();
 });
 it('offers to show hidden completed work when it is everything loaded', async () => {
  h.agenda.mockResolvedValue(response([row({ type: 'completed' })])); render(Agenda); await screen.findByRole('button', { name: '1 completed' }); await toggleCompleted();
  expect(screen.getByText('Completed work is hidden.')).toBeTruthy(); expect(screen.queryByText(/Nothing scheduled/)).toBeNull();
  await fireEvent.click(screen.getByRole('button', { name: 'Show it' })); expect(screen.getByRole('button', { name: '1 completed' })).toBeTruthy();
 });
 it('renders loading, error with retry, and the complete empty state', async () => {
  const pending = deferred<AgendaResponse>(); h.agenda.mockReturnValueOnce(pending.promise); render(Agenda); await screen.findByText('Loading agenda…');
  expect(document.querySelector('.main[aria-busy="true"] .skeleton')).toBeTruthy(); expect(sections()).toHaveLength(0);
  pending.resolve(response([])); await screen.findByText('Nothing scheduled for Thursday 5 November'); expect(document.querySelector('[aria-busy="true"]')).toBeNull();
  cleanup(); h.agenda.mockRejectedValueOnce(new Error('offline')); render(Agenda); await screen.findByText('Could not load this day');
  expect(screen.getAllByRole('button', { name: 'Try again' })).toHaveLength(1); expect(sections()).toHaveLength(0);
  h.agenda.mockResolvedValue(response()); await fireEvent.click(screen.getByRole('button', { name: 'Try again' })); await screen.findByText('First item');
 });
 it('ignores late responses from previous days and Today refetches even on today', async () => {
  const old = deferred<AgendaResponse>(); h.agenda.mockReturnValueOnce(old.promise).mockResolvedValue(response([row({ item_title: 'New day' })]));
  render(Agenda); await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(1));
  await fireEvent.click(screen.getByRole('button', { name: 'Next day' })); await screen.findByText('New day');
  expect(h.agenda.mock.calls[0][0]).not.toBe(h.agenda.mock.calls[1][0]); old.resolve(response()); await settle(); expect(screen.queryByText('First item')).toBeNull();
  await fireEvent.click(screen.getByRole('button', { name: 'Today' })); await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(3));
  await fireEvent.click(screen.getByRole('button', { name: 'Today' })); await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(4));
 });
 it.each([null, { id: 'user-b', timezone: 'UTC' }])('drops pending results after sign-out or identity change: %j', async user => {
  const pending = deferred<AgendaResponse>(); h.agenda.mockReturnValueOnce(pending.promise); render(Agenda); await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(1));
  h.epoch++; h.user = user; pending.resolve(response()); await settle(); expect(screen.queryByText('First item')).toBeNull();
 });
 it('offers controls only on outstanding schedules and disables legacy hard-deadline Move', async () => {
  const entries = ['scheduled', 'done', 'skipped', 'moved', 'missed', 'unrecorded'].map((state, i) => row({ state: state as AgendaEntry['state'], item_ref: `EX-${i}`, schedule_id: `s${i}` }));
  h.agenda.mockResolvedValue(response([...entries, ...['date', 'created', 'completed'].map(type => row({ type: type as AgendaEntry['type'] })), row({ roles: { target: null, earliest: null, deadline: '2031-03-10T09:00:00Z', deadline_behavior: null, occupied_until: null, outcome: '', roles_origin: 'native' }, roles_origin: 'legacy_backfill' })]));
  render(Agenda); await screen.findByRole('button', { name: 'Done EX-30' }); expect(screen.getAllByRole('button', { name: /^Done / })).toHaveLength(1);
  await menu(); expect((screen.getByRole('menuitem', { name: 'Legacy hard deadlines cannot be moved.' }) as HTMLButtonElement).disabled).toBe(true);
 });
 it('offers Move for a native hard deadline without a legacy note', async () => {
  h.agenda.mockResolvedValue(response([row({ roles: { target: null, earliest: null, deadline: '2031-03-10T09:00:00Z', deadline_behavior: null, occupied_until: null, outcome: '', roles_origin: 'native' }, roles_origin: 'native' })]));
  await mount(); await menu();
  expect((screen.getByRole('menuitem', { name: 'Move target…' }) as HTMLButtonElement).disabled).toBe(false);
  expect(screen.queryByRole('menuitem', { name: 'Legacy hard deadlines cannot be moved.' })).toBeNull();
 });
});

describe('resolve identity and dialogs', () => {
 beforeEach(() => localStorage.setItem('pad-agenda-mode:user-a', 'day'));
 it('Done fetches the view at submit and uses the frozen instant, exactly once', async () => {
  await mount(); expect(h.view).not.toHaveBeenCalled(); await fireEvent.click(screen.getByRole('button', { name: 'Done EX-30' }));
  await waitFor(() => expect(h.resolve).toHaveBeenCalledWith('work', 'EX-30', { occurrence_at: frozenAt, state: 'done' }));
  expect(h.resolve).toHaveBeenCalledTimes(1); expect(h.view).toHaveBeenCalledWith('work', 'EX-30');
  await said('Occurrence marked done.', 'success');
  // The toast container is itself a live region, so the page status must not say it again.
  expect(screen.getByRole('status')).toHaveTextContent(''); expect(screen.queryByText('Occurrence marked done.')).toBeNull();
 });
 it('Skip submits once with the view identity', async () => {
  await mount(); await menu(); expect(h.view).not.toHaveBeenCalled(); await fireEvent.click(screen.getByRole('menuitem', { name: 'Skip' }));
  await waitFor(() => expect(h.resolve).toHaveBeenCalledWith('work', 'EX-30', { occurrence_at: frozenAt, state: 'skip' })); await said('Occurrence skipped.', 'success');
 });
 it.each(['different wall', 'ended', 'missing occurrence', '404'])('refuses a %s view as stale', async kind => {
  if (kind === '404') h.view.mockRejectedValue({ code: 'not_found' });
  else h.view.mockResolvedValue({ ended_at: kind === 'ended' ? '2026-11-05T12:00:00Z' : undefined, next_occurrence: kind === 'missing occurrence' ? undefined : { at: frozenAt, wall: kind === 'different wall' ? '2026-11-06T09:00:00' : row().wall } });
  await mount(); await fireEvent.click(screen.getByRole('button', { name: 'Done EX-30' })); await said(staleCopy);
  expect(h.resolve).not.toHaveBeenCalled(); await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(2)); expect(screen.queryByRole('alert')).toBeNull();
 });
 it('focuses the dialog and returns focus to Actions on cancel', async () => {
  await mount(); const trigger = screen.getByRole('button', { name: 'Actions EX-30' }); trigger.focus();
  const d = await dialog('Snooze'); await settle();
  expect(d.contains(document.activeElement)).toBe(true);
  await fireEvent.click(within(d).getByRole('button', { name: 'Cancel' })); await settle();
  expect(document.activeElement).toBe(trigger); expect(h.view).not.toHaveBeenCalled();
 });
 it('Move target accepts an optional reason and uses the preview revision', async () => {
  await mount(); const d = await dialog('Move'); await fillTime(d);
  expect(within(d).getByText(/Will be.*09:30.*GMT-05:00/)).toBeTruthy();
  expect(within(d).getByText('The reminder follows the target.')).toBeTruthy();
  expect(within(d).getByLabelText('Reason (optional)').hasAttribute('required')).toBe(false);
  await fireEvent.click(within(d).getByRole('button', { name: 'Move target' }));
  await waitFor(() => expect(h.move).toHaveBeenCalledWith('work', 'EX-30', { role: 'target', to: '2026-11-06T14:30:00.000Z', revision: 'revision-1' }));
  expect(h.view).not.toHaveBeenCalled(); expect(h.resolve).not.toHaveBeenCalled();
 });
 it('Move target sends a trimmed optional reason', async () => {
  await mount(); const d = await dialog('Move'); await fillTime(d);
  await fireEvent.input(within(d).getByLabelText('Reason (optional)'), { target: { value: ' Waiting for review ' } });
  await fireEvent.click(within(d).getByRole('button', { name: 'Move target' }));
  await waitFor(() => expect(h.move).toHaveBeenCalledWith('work', 'EX-30', { role: 'target', to: '2026-11-06T14:30:00.000Z', revision: 'revision-1', reason: 'Waiting for review' }));
 });
 it('Snooze sends until with no comment and retains input on failure', async () => {
  h.resolve.mockRejectedValue(new Error('offline')); await mount(); const d = await dialog('Snooze'); expect(h.view).not.toHaveBeenCalled(); await fillTime(d);
  await fireEvent.click(within(d).getByRole('button', { name: 'Snooze' }));
  await waitFor(() => expect(h.resolve).toHaveBeenCalledWith('work', 'EX-30', { occurrence_at: frozenAt, state: 'snooze', until: '2026-11-06T14:30:00.000Z' }));
  await screen.findByText('Could not update this occurrence. Try again.'); expect((within(d).getByLabelText(/Remind me again at/) as HTMLInputElement).value).toBe('2026-11-06T09:30');
 });
 it.each([['', 'Choose a time in the future.'], ['2026-11-04T09:30', 'Choose a time in the future.'], ['2028-01-01T09:30', 'Choose a time within a year.']])('rejects invalid time %s before fetching', async (value, message) => {
  await mount(); const d = await dialog('Snooze'); await fillTime(d, value); await fireEvent.click(within(d).getByRole('button', { name: 'Snooze' }));
  expect(within(d).getByText(message)).toBeTruthy(); expect(h.view).not.toHaveBeenCalled(); expect(h.resolve).not.toHaveBeenCalled();
 });
 it('Saving spans both requests and disables only its row', async () => {
  const view = deferred<unknown>(); const write = deferred<unknown>(); h.view.mockReturnValueOnce(view.promise); h.resolve.mockReturnValueOnce(write.promise);
  h.agenda.mockResolvedValue(response([row(), row({ item_ref: 'EX-31', schedule_id: 's2', item_title: 'Second item' })])); await mount();
  await fireEvent.click(screen.getByRole('button', { name: 'Done EX-30' })); await screen.findByText('Saving…');
  for (const name of ['Done EX-30', 'Actions EX-30']) expect((screen.getByRole('button', { name }) as HTMLButtonElement).disabled).toBe(true);
  expect((screen.getByRole('button', { name: 'Done EX-31' }) as HTMLButtonElement).disabled).toBe(false);
  view.resolve({ next_occurrence: { at: frozenAt, wall: row().wall } }); await waitFor(() => expect(h.resolve).toHaveBeenCalledTimes(1)); expect(screen.getByText('Saving…')).toBeTruthy();
  write.resolve({}); await said('Occurrence marked done.', 'success');
 });
 it.each([['resolve', 'stale', 'This occurrence changed. Review the updated entry and try again.'], ['resolve', 'forbidden', 'You can no longer change this item.'], ['view', 'forbidden', 'You can no longer change this item.'], ['view', 'internal', 'Could not update this occurrence. Try again.']])('handles %s %s without replay', async (source, code, message) => {
  h[source === 'view' ? 'view' : 'resolve'].mockRejectedValue({ code }); await mount(); await fireEvent.click(screen.getByRole('button', { name: 'Done EX-30' })); await said(message);
  if (code !== 'internal') await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(2));
  expect(h.resolve).toHaveBeenCalledTimes(source === 'view' ? 0 : 1);
 });
 it('shows the server sentence when a skip before a hard deadline needs the owner, and keeps the button', async () => {
  const sentence = 'Only the assignee or a workspace owner can skip this occurrence before its hard deadline.';
  h.resolve.mockRejectedValue({ code: 'deadline_owner_required', message: sentence }); await mount(); await fireEvent.click(screen.getByRole('button', { name: 'Done EX-30' })); await said(sentence);
  expect(h.agenda).toHaveBeenCalledTimes(1); expect((screen.getByRole('button', { name: 'Done EX-30' }) as HTMLButtonElement).disabled).toBe(false);
 });
 it('keeps rows on quiet refresh and retries only the read after a successful write', async () => {
  const refresh = deferred<AgendaResponse>(); h.agenda.mockResolvedValueOnce(response()).mockReturnValueOnce(refresh.promise); await mount();
  await fireEvent.click(screen.getByRole('button', { name: 'Done EX-30' })); await screen.findByText('Updating agenda…'); expect(screen.getByText('First item')).toBeTruthy();
  refresh.resolve(response()); await said('Occurrence marked done.', 'success'); await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent(''));
  h.agenda.mockRejectedValueOnce(new Error('offline')); await fireEvent.click(screen.getByRole('button', { name: 'Done EX-30' })); await screen.findByText('Saved. Could not refresh the view.');
  const writes = h.resolve.mock.calls.length; h.agenda.mockResolvedValue(response()); await fireEvent.click(screen.getByRole('button', { name: 'Try again' })); await settle(); expect(h.resolve).toHaveBeenCalledTimes(writes);
 });
 it('ack keeps the row outstanding and refreshes', async () => {
  const acknowledged = row({ fired: true, state: 'acknowledged', acknowledged_at: '2026-11-05T14:05:00Z', acknowledged_by: { user_id: 'user-a', name: 'Alice', actor: 'user' } });
  h.agenda.mockResolvedValueOnce(response([row({ fired: true })])).mockResolvedValue(response([acknowledged]));
  h.resolve.mockResolvedValue({ commitment: {}, resolution: { occurrence_at: frozenAt, state: 'ack', missed_recorded: 0 } });
  await mount(); await fireEvent.click(screen.getByRole('button', { name: 'Acknowledge EX-30' }));
  await waitFor(() => expect(h.resolve).toHaveBeenCalledWith('work', 'EX-30', { occurrence_at: frozenAt, state: 'ack' }));
  expect(h.resolve).toHaveBeenCalledTimes(1); expect(h.view).toHaveBeenCalledWith('work', 'EX-30');
  await said('Acknowledged. The work stays outstanding.', 'success');
  expect(h.toast.mock.calls.map(call => call[0])).not.toContain('Occurrence marked done.');
  expect(h.agenda).toHaveBeenCalledTimes(2);
  await screen.findByText('Acknowledged by Alice · Thu 5, 09:05');
  const item = screen.getByRole('link', { name: 'First item' }).closest('li')!;
  expect(item.classList.contains('closed')).toBe(false); expect(item.classList.contains('done')).toBe(false);
  expect(screen.queryByRole('button', { name: 'Acknowledge EX-30' })).toBeNull();
  expect(screen.getByRole('button', { name: 'Done EX-30' })).toBeEnabled();
  expect(screen.queryByText(/Undo/)).toBeNull();
 });
 it('offers no Acknowledge on an occurrence that has not fired', async () => {
  await mount(); expect(screen.queryByRole('button', { name: 'Acknowledge EX-30' })).toBeNull();
 });
 it('an acknowledged occurrence can still be marked done', async () => {
  h.agenda.mockResolvedValue(response([row({ fired: true, state: 'acknowledged', acknowledged_at: '2026-11-05T14:05:00Z' })]));
  await mount(); await fireEvent.click(screen.getByRole('button', { name: 'Done EX-30' }));
  await waitFor(() => expect(h.resolve).toHaveBeenCalledWith('work', 'EX-30', { occurrence_at: frozenAt, state: 'done' }));
  await said('Occurrence marked done.', 'success');
 });
 it.each([
  ['not_fired', 'This occurrence has not fired yet, so there is nothing to acknowledge.'],
  ['stale', staleCopy]
 ])('an ack refused as %s refreshes and explains, without retrying', async (code, message) => {
  h.agenda.mockResolvedValue(response([row({ fired: true })]));
  h.resolve.mockRejectedValue({ code }); await mount(); await fireEvent.click(screen.getByRole('button', { name: 'Acknowledge EX-30' }));
  await said(message); await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(2));
  expect(h.resolve).toHaveBeenCalledTimes(1);
 });
 it('treats a pinned occurrence sharing the wall time but not the instant as stale', async () => {
  // A DST fall-back hour: the row names an earlier instant at the same wall time
  // as the schedule's current occurrence.
  const earlier = new Date(Date.parse(frozenAt) - 3600_000).toISOString();
  h.agenda.mockResolvedValue(response([row({ fired: true, occurrence_key: `${earlier}|${row().wall}` })]));
  h.view.mockResolvedValue({ next_occurrence: { at: frozenAt, wall: row().wall } });
  await mount(); await fireEvent.click(screen.getByRole('button', { name: 'Acknowledge EX-30' }));
  await said(staleCopy); expect(h.resolve).not.toHaveBeenCalled();
 });
 it('resolves a pinned occurrence whose key names the current instant', async () => {
  h.agenda.mockResolvedValue(response([row({ fired: true, occurrence_key: `${frozenAt}|${row().wall}` })]));
  h.view.mockResolvedValue({ next_occurrence: { at: frozenAt, wall: row().wall } });
  h.resolve.mockResolvedValue({ commitment: {}, resolution: { occurrence_at: frozenAt, state: 'ack', missed_recorded: 0 } });
  await mount(); await fireEvent.click(screen.getByRole('button', { name: 'Acknowledge EX-30' }));
  await waitFor(() => expect(h.resolve).toHaveBeenCalledWith('work', 'EX-30', { occurrence_at: frozenAt, state: 'ack' }));
 });
 it('does not resolve after identity changes during the schedule read', async () => {
  const view = deferred<unknown>(); h.view.mockReturnValueOnce(view.promise); await mount(); await fireEvent.click(screen.getByRole('button', { name: 'Done EX-30' }));
  h.epoch++; view.resolve({ next_occurrence: { at: frozenAt, wall: row().wall } }); await settle(); expect(h.resolve).not.toHaveBeenCalled();
 });
});

describe('agenda Week and priority', () => {
 it('offers Day, Week and Month as one group with exactly one selected, switched by arrow keys', async () => {
  h.agenda.mockResolvedValue(response([])); render(Agenda);
  await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(1));
  const range = within(screen.getByRole('group', { name: 'Range' }));
  const pressed = () => range.getAllByRole('button').filter(button => button.getAttribute('aria-pressed') === 'true').map(button => button.textContent?.trim());
  expect(range.getAllByRole('button').map(button => button.textContent?.trim())).toEqual(['Day', 'Week', 'Month']);
  expect(pressed()).toEqual(['Week']);
  await fireEvent.keyDown(range.getByRole('button', { name: 'Week' }), { key: 'ArrowRight' });
  await waitFor(() => expect(pressed()).toEqual(['Month']));
  expect(range.getByRole('button', { name: 'Month' })).toHaveFocus();
  expect(screen.getByRole('button', { name: 'Next month' })).toBeInTheDocument();
 });
 it('an incomplete week never says a day is empty', async () => {
  h.agenda.mockResolvedValue(response([], { window_complete: false, truncated: true }));
  render(Agenda);
  await screen.findByText('Too much to load at once. Choose a smaller range or a workspace group.');
  expect(screen.queryByRole('button', { name: 'Load the rest' })).toBeNull();
  expect(stripDays()).toHaveLength(7); expect(screen.queryByText(/Nothing scheduled/)).toBeNull();
 });
 it('defaults to one seven-local-day DST window, renders seven days and shifts by civil weeks', async () => {
  vi.mocked(Date.now).mockReturnValue(Date.parse('2026-10-30T16:00:00Z'));
  h.agenda.mockResolvedValue(response([])); render(Agenda);
  await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(1));
  expect(h.agenda.mock.calls[0]).toEqual(['2026-10-30T04:00:00.000Z', '2026-11-06T05:00:00.000Z', 'America/New_York', 'all', ['completed']]);
  expect(screen.getByRole('button', { name: 'Week' })).toHaveAttribute('aria-pressed', 'true');
  expect(stripDays().map(day => day.dataset.stripDay)).toEqual(['2026-10-30', '2026-10-31', '2026-11-01', '2026-11-02', '2026-11-03', '2026-11-04', '2026-11-05']);
  expect(stripDays().filter(day => day.getAttribute('aria-current') === 'date')).toEqual([stripDay('2026-10-30')]);
  expect(await screen.findAllByText(/^Nothing scheduled/)).toHaveLength(1); expect(sections()).toHaveLength(0);
  expect(screen.getByText('Nothing scheduled for 30 October – 5 November 2026')).toBeTruthy();
  await fireEvent.click(screen.getByRole('button', { name: 'Next week' }));
  await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(2));
  expect(h.agenda.mock.calls[1]).toEqual(['2026-11-06T05:00:00.000Z', '2026-11-13T05:00:00.000Z', 'America/New_York', 'all', ['completed']]);
  expect(screen.getByRole('heading', { name: '6–12 November 2026' })).toBeTruthy();
  await fireEvent.click(screen.getByRole('button', { name: 'Previous week' }));
  await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(3));
  expect(h.agenda.mock.calls[2]).toEqual(h.agenda.mock.calls[0]);
 });
 it('honours saved Day mode, persists the mode per user and keeps the selected date when switching', async () => {
  localStorage.setItem('pad-agenda-mode:user-a', 'day'); await mount();
  expect(screen.getByRole('button', { name: 'Day' })).toHaveAttribute('aria-pressed', 'true');
  await fireEvent.click(screen.getByRole('button', { name: 'Next day' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Week' }));
  expect(localStorage.getItem('pad-agenda-mode:user-a')).toBe('week');
  await waitFor(() => expect(h.agenda.mock.lastCall?.[0]).toBe('2026-11-06T05:00:00.000Z'));
  expect(screen.getByRole('heading', { name: '6–12 November 2026' })).toBeTruthy();
  await fireEvent.click(screen.getByRole('button', { name: 'Day' }));
  expect(localStorage.getItem('pad-agenda-mode:user-a')).toBe('day');
  h.user = { id: 'user-b', timezone: 'UTC' }; h.epoch++; h.signal?.set('auth', 1);
  await waitFor(() => expect(screen.getByRole('button', { name: 'Week' })).toHaveAttribute('aria-pressed', 'true'));
  expect(localStorage.getItem('pad-agenda-mode:user-b')).toBeNull();
 });
 it('cheap drag keeps the wall clock, uses the preview revision once and keeps the week', async () => {
  await mount(); await drop();
  await waitFor(() => expect(h.move).toHaveBeenCalledWith('work', 'EX-30', { role: 'target', to: '2026-11-06T14:00:00.000Z', revision: 'revision-1' }));
  expect(h.preview).toHaveBeenCalledTimes(1); expect(h.move).toHaveBeenCalledTimes(1);
  expect(h.preview).toHaveBeenCalledWith('work', 'EX-30', { role: 'target', to: '2026-11-06T14:00:00.000Z' });
  expect(screen.queryByRole('dialog')).toBeNull(); expect(screen.getByRole('status')).not.toHaveTextContent(/Occurrence moved/);
  await said('Occurrence moved to November 6, 2026, 09:00. Its reminder follows the target.', 'success');
  expect(h.agenda.mock.calls[1]).toEqual(h.agenda.mock.calls[0]);
  expect(screen.getByRole('button', { name: 'Week' })).toHaveAttribute('aria-pressed', 'true');
  expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Actions EX-30' }));
 });
 it('previews a linked child once per day and renders linked ghosts at their proposed day', async () => {
  const child = row({ item_ref: 'CH-2', item_title: 'Child item' });
  h.agenda.mockResolvedValue(response([child]));
  const change = { item_id: 'linked', role: 'deadline' as const, to: '2026-11-07T15:00:00Z' };
  h.preview.mockResolvedValue(preview(undefined, { changes: [...preview().changes, change], violations: [change] }));
  render(Agenda); await screen.findByText('Child item'); await drag('2026-11-06', 'CH-2');
  await waitFor(() => expect(within(daySection('2026-11-07')).getByText('Linked child (linked) · proposed').closest('[aria-hidden="true"]')).toBeTruthy());
  expect(within(daySection('2026-11-07')).getByText('Outside its bound')).toBeTruthy();
  await fireEvent.dragEnter(daySection()); expect(h.preview).toHaveBeenCalledTimes(1);
  await fireEvent.drop(daySection());
  const d = await screen.findByRole('dialog'); await fireEvent.click(within(d).getByRole('checkbox')); await fireEvent.click(within(d).getByRole('button', { name: 'Move target' }));
  await waitFor(() => expect(h.move).toHaveBeenCalledWith('work', 'CH-2', expect.objectContaining({ role: 'target', acknowledge_violations: true })));
 });
 it.each(['different', 'missing'])('rejects a %s preview root and reloads without a write', async kind => {
  h.preview.mockResolvedValue(preview(undefined, { changes: kind === 'missing' ? [] : [{ ...preview().changes[0], from: '2026-11-04T14:00:00Z' }] }));
  await mount(); await drop(); await said(staleCopy);
  expect(h.move).not.toHaveBeenCalled(); await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(2));
 });
 it.each(['preview', 'move'])('handles commitment_changed from %s without retry', async source => {
  h[source].mockRejectedValue({ code: 'commitment_changed' }); await mount(); await drop();
  await said(staleCopy);
  expect(h.move).toHaveBeenCalledTimes(source === 'move' ? 1 : 0); expect(h.preview).toHaveBeenCalledTimes(1);
  await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(2));
 });
 it('reviews every change, requires a deadline reason and lists finished dates and role labels', async () => {
  const linked = { item_id: 'linked', role: 'earliest' as const, to: '2026-11-07T15:00:00Z' };
  h.preview.mockResolvedValue(preview(undefined, { renegotiates_deadline: true, changes: [linked, ...preview().changes], left_in_place: [{ ...linked, role: 'occupied_until' }] }));
  await mount(); await drag(); await screen.findByText('Review needed: this move changes a hard deadline.'); await fireEvent.drop(daySection());
  const d = await screen.findByRole('dialog', { name: 'Review move' });
  expect(d.querySelector('.subject')).toHaveTextContent('First item · First workspace EX-30');
  expect(within(d).getByText('This move changes the hard deadline.')).toBeTruthy();
  await within(d).findAllByText('Linked child');
  const moves = [...d.querySelectorAll('[data-change]')].map(el => el.textContent?.replace(/\s+/g, ' ').trim());
  expect(moves).toEqual(['First item Target Thu 5, 09:00 → Fri 6, 09:00', 'Linked child Linked · Available from → Sat 7, 10:00']);
  expect(within(d).getByText('Not moved (finished)')).toBeTruthy();
  expect([...d.querySelectorAll('[data-left]')].map(el => el.textContent?.replace(/\s+/g, ' ').trim())).toEqual(['Linked child Linked · Until → Sat 7, 10:00']);
  await fireEvent.click(within(d).getByRole('button', { name: 'Move and change deadline' }));
  expect(within(d).getByText('Enter a reason.')).toBeTruthy(); expect(h.move).not.toHaveBeenCalled();
  await fireEvent.input(within(d).getByLabelText('Reason'), { target: { value: ' Agreed later ' } });
  await fireEvent.click(within(d).getByRole('button', { name: 'Move and change deadline' }));
  await waitFor(() => expect(h.move).toHaveBeenCalledWith('work', 'EX-30', { role: 'target', to: '2026-11-06T14:00:00.000Z', revision: 'revision-1', reason: 'Agreed later' }));
 });
 it.each(['Cancel', 'Escape'])('%s closes review with no write', async method => {
  h.preview.mockResolvedValue(preview(undefined, { renegotiates_deadline: true })); await mount(); await drop(); const d = await screen.findByRole('dialog');
  if (method === 'Cancel') await fireEvent.click(within(d).getByRole('button', { name: 'Cancel' }));
  else await fireEvent(d, new Event('cancel', { cancelable: true }));
  expect(screen.queryByRole('dialog')).toBeNull(); expect(h.move).not.toHaveBeenCalled();
 });
 it('requires bounds acceptance and falls back to Linked item for unavailable titles', async () => {
  const change = { item_id: 'linked', role: 'deadline' as const, to: '2026-11-07T15:00:00Z' };
  h.item.mockRejectedValue(new Error('offline')); h.preview.mockResolvedValue(preview(undefined, { changes: [...preview().changes, change], violations: [change] }));
  await mount(); await drop(); const d = await screen.findByRole('dialog');
  expect(await within(d).findByText('Linked item')).toBeTruthy();
  expect(within(d).getByText('Outside bound')).toBeTruthy();
  await fireEvent.click(within(d).getByRole('button', { name: 'Move target' })); expect(h.move).not.toHaveBeenCalled();
  await fireEvent.click(within(d).getByLabelText('Accept dates outside their bounds'));
  await fireEvent.click(within(d).getByRole('button', { name: 'Move target' }));
  await waitFor(() => expect(h.move).toHaveBeenCalledWith('work', 'EX-30', { role: 'target', to: '2026-11-06T14:00:00.000Z', revision: 'revision-1', acknowledge_violations: true }));
 });
 it('pins available and deadline days while dragging and clears drag decorations on Escape', async () => {
  const roles = { ...row().roles!, target: row().at, earliest: '2026-11-06T14:00:00Z', deadline: '2026-11-08T14:00:00Z' };
  h.agenda.mockResolvedValue(response([row({ roles, roles_origin: 'native' })])); await mount(); await drag('2026-11-07');
  expect(within(stripDay()).getByText('Available from', { exact: true })).toBeTruthy();
  expect(within(stripDay('2026-11-08')).getByText('Deadline', { exact: true })).toBeTruthy();
  expect(within(stripDay('2026-11-07')).getByText('Move here')).toBeTruthy(); expect(daySection('2026-11-07').classList.contains('drop')).toBe(true);
  await fireEvent.keyDown(window, { key: 'Escape' });
  expect(screen.queryByText('Available from', { exact: true })).toBeNull(); expect(screen.queryByText('Move here')).toBeNull(); expect(h.move).not.toHaveBeenCalled();
  expect(stripDays().every(day => day.dataset.drop === undefined)).toBe(true);
  await fireEvent.drop(daySection()); await fireEvent.drop(stripDay()); expect(h.move).not.toHaveBeenCalled();
 });
 it('labels target and all date roles, with noninteractive separate-day deadline markers', async () => {
  h.agenda.mockResolvedValue(response([row({ roles: { ...row().roles!, target: row().at, earliest: '2026-11-05T13:00:00Z', deadline: '2026-11-05T16:00:00Z', deadline_behavior: 'stays_owed', occupied_until: '2026-11-05T15:00:00Z' } }), row({ item_ref: 'EX-31', item_title: 'Another item', roles: { ...row().roles!, deadline: '2026-11-08T14:00:00Z', deadline_behavior: 'lapses' } })]));
  await mount(); const first = daySection('2026-11-05').querySelector('li[data-ref="EX-30"]')!;
  expect(first.querySelector('.time')?.textContent).toMatch(/09:00.*from 08:00.*until 10:00/);
  expect(first.querySelector('[data-kind="deadline"]')).toHaveAttribute('title', 'Deadline November 5, 2026, 11:00 · Still owed');
  expect(daySection('2026-11-05').querySelector('li[data-role="deadline"]')).toBeNull();
  const marker = daySection('2026-11-08').querySelector('li[data-role="deadline"]')!;
  expect(marker.textContent).toMatch(/Deadline\s*Another item/);
  expect(within(marker as HTMLElement).queryByRole('button')).toBeNull(); expect(marker.getAttribute('draggable')).not.toBe('true');
  expect(within(daySection('2026-11-08')).queryByText('Nothing scheduled.')).toBeNull();
  expect(within(daySection('2026-11-09')).getByText('Nothing scheduled.')).toBeTruthy();
 });
 it('dropping a card back on its own day is a no-op, not a conflict', async () => {
  await mount(); await drop('2026-11-05');
  await settle();
  expect(h.preview).not.toHaveBeenCalled(); expect(h.move).not.toHaveBeenCalled();
  expect(screen.queryByText(/This occurrence changed/)).toBeNull(); expect(h.agenda).toHaveBeenCalledTimes(1);
 });
 it('date-only roles stay on their own date west of UTC, and an all-day move is not read as stale', async () => {
  const roles = { ...row().roles!, target: '2026-11-05', deadline: '2026-11-08', deadline_behavior: 'lapses' as const };
  h.agenda.mockResolvedValue(response([row({ all_day: true, at: '2026-11-05T05:00:00Z', day: '2026-11-05', wall: '2026-11-05T00:00:00', roles, roles_origin: 'native' })]));
  // The engine resolves a date-only target at midnight in the recipient zone.
  h.preview.mockImplementation((_ws, _ref, body) => Promise.resolve(preview(body.to, { changes: [{ item_id: 'root', role: 'target', from: '2026-11-05T05:00:00Z', to: body.to, root: true }] })));
  await mount();
  expect(daySection('2026-11-08').querySelector('li[data-role="deadline"]')?.textContent).toMatch(/Deadline\s*First item/);
  expect(daySection('2026-11-07').querySelector('li[data-role="deadline"]')).toBeNull();
  expect(daySection('2026-11-05').querySelector('[data-kind="deadline"]')).toHaveAttribute('title', 'Deadline November 8, 2026 · Lapses');
  await drop('2026-11-06');
  await waitFor(() => expect(h.move).toHaveBeenCalledTimes(1));
  expect(h.move.mock.calls[0][2]).toMatchObject({ role: 'target', to: '2026-11-06T05:00:00.000Z', revision: 'revision-1' });
  expect(screen.queryByText(/This occurrence changed/)).toBeNull();
 });
 it('Move target to the current target closes the dialog with no request', async () => {
  h.agenda.mockResolvedValue(response([row({ at: '2026-11-06T14:00:00Z', day: '2026-11-06', wall: '2026-11-06T09:00:00', roles: { ...row().roles!, target: '2026-11-06T14:00:00Z' } })]));
  await mount(); await menu();
  await fireEvent.click(screen.getByRole('menuitem', { name: 'Move target…' }));
  const d = screen.getByRole('dialog', { name: 'Move target' });
  await fillTime(d, '2026-11-06T09:00'); await fireEvent.submit(d.querySelector('form')!); await settle();
  expect(h.preview).not.toHaveBeenCalled(); expect(h.move).not.toHaveBeenCalled();
  expect(screen.queryByRole('dialog', { name: 'Move target' })).toBeNull();
  expect(screen.queryByText(/This occurrence changed/)).toBeNull();
 });
 it('Week says when the Show completed toggle hides every entry', async () => {
  localStorage.setItem('pad-agenda-completed:user-a', '0');
  h.agenda.mockResolvedValue(response([row({ type: 'completed', item_title: 'Completed item' })]));
  render(Agenda);
  expect(await screen.findByText('Completed work is hidden.')).toBeTruthy(); expect(screen.queryByText(/Nothing scheduled/)).toBeNull();
  await fireEvent.click(screen.getByRole('button', { name: 'Show it' }));
  await fireEvent.click(await screen.findByRole('button', { name: '1 completed' })); expect(screen.getByText('Completed item')).toBeTruthy();
 });
 it('deadline markers come only from live occurrences, never history rows', async () => {
  const roles = { ...row().roles!, deadline: '2026-11-08T14:00:00Z', deadline_behavior: 'lapses' as const };
  h.agenda.mockResolvedValue(response([
   row({ item_ref: 'EX-31', item_title: 'Another item', roles }),
   row({ item_ref: 'EX-31', item_title: 'Another item', roles, state: 'moved', at: '2026-11-04T14:00:00Z', day: '2026-11-04', wall: '2026-11-04T09:00:00' }),
   row({ item_ref: 'EX-32', item_title: 'Finished item', roles, state: 'done', at: '2026-11-06T14:00:00Z', day: '2026-11-06', wall: '2026-11-06T09:00:00' })
  ]));
  render(Agenda); await screen.findAllByText('Another item');
  const markers = [...daySection('2026-11-08').querySelectorAll('li[data-role="deadline"]')];
  expect(markers).toHaveLength(1); expect(markers[0].textContent).toContain('Another item');
 });
 it.each([false, true])('Bring to today uses the shared target flow, all-day=%s', async all_day => {
  const at = all_day ? '2026-11-06T05:00:00Z' : '2026-11-06T14:00:00Z';
  h.agenda.mockResolvedValue(response([row({ all_day, at, day: '2026-11-06', wall: all_day ? '2026-11-06T00:00:00' : '2026-11-06T09:00:00' })]));
  h.preview.mockImplementation((_ws, _ref, body) => Promise.resolve(preview(body.to, { changes: [{ item_id: 'root', role: 'target', from: at, to: body.to, root: true }] })));
  await mount(); await menu();
  await fireEvent.click(screen.getByRole('menuitem', { name: 'Bring to today' }));
  await waitFor(() => expect(h.move).toHaveBeenCalledWith('work', 'EX-30', { role: 'target', to: all_day ? '2026-11-05T05:00:00.000Z' : '2026-11-05T14:00:00.000Z', revision: 'revision-1' }));
  expect(h.resolve).not.toHaveBeenCalled();
 });
 it('has only one draggable occurrence with a moved history row and no extra quantitative fields', async () => {
  h.agenda.mockResolvedValue(response([row({ state: 'moved' }), row({ at: '2026-11-06T14:00:00Z', day: '2026-11-06' })])); render(Agenda);
  await waitFor(() => expect(document.querySelectorAll('[draggable="true"]')).toHaveLength(1));
  expect(screen.getByText('Moved')).toBeTruthy();
  expect(screen.queryByText(/size|effort|capacity/i)).toBeNull(); expect(screen.queryByLabelText(/size|effort|capacity/i)).toBeNull();
 });
 it('lists outstanding work in the rail: ranks in order, chips, short dates and group completeness', async () => {
  h.agenda.mockResolvedValue(response([], { priority_list: [workRow({ item_title: 'High B', rank: 'high', reason: 'target_passed' }), workRow({ item_ref: 'EX-31', item_title: 'High A', rank: 'high', reason: 'deadline_still_owed' }), workRow({ item_ref: 'EX-32', item_title: 'Normal row', roles: { ...row().roles!, target: row().at, deadline: '2026-11-06T14:00:00Z' } }), workRow({ item_ref: 'EX-33', item_title: 'Low row', rank: 'low' }), workRow({ item_ref: 'EX-34', item_title: 'Unranked row', rank: undefined })], waiting_on: [workRow({ type: 'item', item_ref: 'W-1', item_title: 'Blocked work', reason: 'blocked_needs_decision' }), workRow({ type: 'item', item_ref: 'W-2', item_title: 'Waiting work', reason: 'waiting' })], no_date_complete: false })); render(Agenda);
  const list = await screen.findByRole('region', { name: 'Outstanding' });
  expect(list.closest('aside')).toBeTruthy();
  expect(within(list).getAllByRole('heading').map(el => el.textContent?.replace(/\s+/g, ' ').trim())).toEqual(['Outstanding', 'High 2', 'Normal 1', 'Low 1', 'Unranked 1']);
  expect(within(list).getAllByRole('tab').map(el => el.textContent?.replace(/\s+/g, ' ').trim())).toEqual(['By priority 5', 'Waiting 2', 'No date 0+']);
  expect(within(list).getAllByRole('link').slice(0, 2).map(el => el.textContent)).toEqual(['High B', 'High A']);
  for (const text of ['Your work, any date', 'Still owed', 'Deadline passed']) expect(within(list).getByText(text)).toBeTruthy();
  const normal = list.querySelector('li[data-work="EX-32"]') as HTMLElement;
  expect(normal.querySelector('.when')?.textContent).toBe('Thu 5, 09:00'); expect(normal.querySelector('[data-kind="deadline"]')?.textContent).toContain('Fri 6, 09:00');
  expect(within(list).queryByRole('button', { name: /^Done/ })).toBeNull();
  await fireEvent.click(within(list).getByRole('tab', { name: 'Waiting 2' }));
  expect(within(list).getByText('Needs a decision')).toBeTruthy(); expect(within(list).queryByRole('button', { name: /^(Done|Actions)/ })).toBeNull();
  await fireEvent.click(within(list).getByRole('tab', { name: 'No date 0+' }));
  expect(within(list).getByText('This list is incomplete.')).toBeTruthy(); expect(within(list).queryByText('No undated work.')).toBeNull();
  expect(screen.queryByText(/Showing entries up to|Too much to load|could not be shown/)).toBeNull();
 });
 it('puts the summary first in the rail and the outstanding list under it, in Week', async () => {
  h.agenda.mockResolvedValue(response([row()], { priority_list: [workRow()], counts: [{ day: '2026-11-05', completed: 1, open: 2, burndown: 2 }] })); await mount();
  const rail = document.querySelector('aside.rail') as HTMLElement;
  expect(within(rail).getAllByRole('region').map(el => el.getAttribute('aria-labelledby') && document.getElementById(el.getAttribute('aria-labelledby')!)?.textContent)).toEqual(['This week', 'Outstanding']);
  expect(rail.querySelector('[data-stat="outstanding"] b')?.textContent).toBe('1'); expect(rail.querySelector('[data-stat="completed"] b')?.textContent).toBe('1');
 });
 it('Day range keeps the summary in the rail, named for the day, without the outstanding list', async () => {
  localStorage.setItem('pad-agenda-mode:user-a', 'day');
  h.agenda.mockResolvedValue(response([row()], { priority_list: [workRow()] })); await mount();
  expect(within(document.querySelector('aside.rail') as HTMLElement).getByRole('region', { name: 'This day' })).toBeTruthy();
  expect(screen.queryByRole('region', { name: 'Outstanding' })).toBeNull();
 });
 it('the rail is a skeleton while the agenda loads and says so when it did not load', async () => {
  const first = deferred<AgendaResponse>(); h.agenda.mockReturnValueOnce(first.promise); render(Agenda);
  await waitFor(() => expect(document.querySelectorAll('aside.rail .rail-skeleton')).toHaveLength(2));
  expect(screen.queryByRole('region', { name: 'Outstanding' })).toBeNull(); expect(screen.queryByText('Not loaded.')).toBeNull();
  first.resolve(response()); await screen.findByRole('region', { name: 'Outstanding' }); expect(document.querySelector('.rail-skeleton')).toBeNull();
  h.agenda.mockRejectedValue(new Error('offline')); await fireEvent.click(screen.getByRole('button', { name: 'Next week' }));
  await screen.findByText('Could not load this week');
  expect(within(document.querySelector('aside.rail') as HTMLElement).getByText('Not loaded.')).toBeTruthy();
  expect(screen.queryByRole('region', { name: 'Outstanding' })).toBeNull(); expect(screen.queryByRole('region', { name: 'This week' })).toBeNull();
 });
 it('an empty week still lists outstanding work in the rail', async () => {
  h.agenda.mockResolvedValue(response([], { no_date: [workRow({ type: 'item', item_title: 'Undated' })] })); render(Agenda);
  await screen.findByText(/Nothing scheduled for/);
  const list = screen.getByRole('region', { name: 'Outstanding' });
  expect(within(list).getByText('Nothing outstanding.')).toBeTruthy();
  await fireEvent.click(within(list).getByRole('tab', { name: 'No date 1' })); expect(within(list).getByText('Undated')).toBeTruthy();
 });
 it('renders complete empty groups and keeps window incompleteness independent', async () => {
  h.agenda.mockResolvedValue(response([], { window_complete: false })); render(Agenda);
  for (const text of ['Nothing outstanding.', 'Too much to load at once. Choose a smaller range or a workspace group.']) expect(await screen.findByText(text)).toBeTruthy();
  await fireEvent.click(screen.getByRole('tab', { name: 'Waiting 0' })); expect(screen.getByText('Nothing waiting.')).toBeTruthy();
  await fireEvent.click(screen.getByRole('tab', { name: 'No date 0' })); expect(screen.getByText('No undated work.')).toBeTruthy();
 });
 it('never calls an incomplete empty group empty', async () => {
  h.agenda.mockResolvedValue(response([], { priority_list_complete: false, waiting_on_complete: false, no_date_complete: false })); render(Agenda);
  await screen.findByRole('region', { name: 'Outstanding' });
  for (const name of ['By priority 0+', 'Waiting 0+', 'No date 0+']) {
   await fireEvent.click(screen.getByRole('tab', { name }));
   expect(screen.getAllByText('This list is incomplete.')).toHaveLength(1);
   for (const text of ['Nothing outstanding.', 'Nothing waiting.', 'No undated work.']) expect(screen.queryByText(text)).toBeNull();
  }
 });
 it('moves list schedules through their own target and ref, without Done', async () => {
  h.agenda.mockResolvedValue(response([], { priority_list: [workRow({ day: undefined, at: undefined, roles: { ...row().roles!, target: row().at } })] })); render(Agenda);
  const list = await screen.findByRole('region', { name: 'Outstanding' });
  expect(within(list).queryByRole('button', { name: /^Done/ })).toBeNull();
  await fireEvent.click(within(list).getByRole('button', { name: 'Actions for EX-30' }));
  await fireEvent.click(screen.getByRole('menuitem', { name: 'Move target…' }));
  const d = screen.getByRole('dialog', { name: 'Move target' }); await fillTime(d, '2026-11-06T09:00');
  await fireEvent.click(within(d).getByRole('button', { name: 'Move target' }));
  await waitFor(() => expect(h.move).toHaveBeenCalledWith('work', 'EX-30', { role: 'target', to: '2026-11-06T14:00:00.000Z', revision: 'revision-1' }));
 });
 it('Bring to today from the rail menu moves the target through the shared flow', async () => {
  const at = '2026-11-07T14:00:00Z';
  h.agenda.mockResolvedValue(response([], { priority_list: [workRow({ day: undefined, at: undefined, roles: { ...row().roles!, target: at } })] }));
  h.preview.mockImplementation((_ws, _ref, body) => Promise.resolve(preview(body.to, { changes: [{ item_id: 'root', role: 'target', from: at, to: body.to, root: true }] })));
  render(Agenda);
  await fireEvent.click(await screen.findByRole('button', { name: 'Actions for EX-30' }));
  await fireEvent.click(screen.getByRole('menuitem', { name: 'Bring to today' }));
  await waitFor(() => expect(h.move).toHaveBeenCalledWith('work', 'EX-30', { role: 'target', to: '2026-11-05T14:00:00.000Z', revision: 'revision-1' }));
 });
 it('a rail row shows a spinner while its move is in flight, and no menu', async () => {
  const held = deferred<CommitmentMoveResult>(); h.preview.mockReturnValue(held.promise);
  h.agenda.mockResolvedValue(response([], { priority_list: [workRow({ day: undefined, at: undefined, roles: { ...row().roles!, target: '2026-11-07T14:00:00Z' } })] })); render(Agenda);
  await fireEvent.click(await screen.findByRole('button', { name: 'Actions for EX-30' }));
  await fireEvent.click(screen.getByRole('menuitem', { name: 'Bring to today' }));
  await waitFor(() => expect(document.querySelector('li[data-work="EX-30"] .spinner')).toBeTruthy());
  expect(screen.queryByRole('button', { name: 'Actions for EX-30' })).toBeNull();
 });
 it('switches between Schedule and Outstanding in place at mobile widths without fetching', async () => {
  vi.spyOn(window, 'matchMedia').mockImplementation(query => ({ matches: query === '(max-width: 768px)', media: query, addEventListener: vi.fn(), removeEventListener: vi.fn() }) as unknown as MediaQueryList);
  vi.spyOn(window, 'scrollTo').mockImplementation(() => {});
  h.agenda.mockResolvedValue(response([row()], { priority_list: [workRow({ item_ref: 'EX-40', item_title: 'Owed item' }), workRow({ item_ref: 'EX-41', item_title: 'Other owed' })], counts: [{ day: '2026-11-05', completed: 1, open: 2, burndown: 2 }] }));
  await mount();
  const tabs = within(screen.getByRole('tablist', { name: 'Show' })).getAllByRole('tab');
  expect(tabs.map(el => el.textContent?.replace(/\s+/g, ' ').trim())).toEqual(['Schedule', 'Outstanding 2']);
  expect(tabs[0]).toHaveAttribute('aria-selected', 'true');
  const panel = () => document.getElementById(tabs[0].getAttribute('aria-controls')!)!;
  expect(panel()).toHaveAttribute('role', 'tabpanel'); expect(panel()).toHaveAttribute('aria-labelledby', tabs[0].id); expect(tabs[1]).toHaveAttribute('aria-controls', panel().id);
  expect(document.querySelector('li[data-ref="EX-30"]')?.classList.contains('compact')).toBe(true);
  // The schedule surface is the schedule alone: the rail belongs to the other tab.
  expect(panel().contains(screen.getByText('First item'))).toBe(true); expect(screen.queryByRole('region', { name: 'Outstanding' })).toBeNull(); expect(screen.queryByRole('region', { name: 'This week' })).toBeNull();
  expect(screen.getByRole('button', { name: 'Range: Week' })).toBeTruthy(); expect(screen.queryByRole('group', { name: 'Range' })).toBeNull();
  await fireEvent.click(tabs[1]);
  expect(tabs[1]).toHaveAttribute('aria-selected', 'true'); expect(panel()).toHaveAttribute('aria-labelledby', tabs[1].id);
  expect(screen.getByRole('region', { name: 'Outstanding' })).toBeVisible(); expect(screen.queryByText('First item')).toBeNull(); expect(stripDays()).toHaveLength(0);
  // Summary first, then the list, both inside the panel.
  expect(within(panel()).getAllByRole('region').map(el => document.getElementById(el.getAttribute('aria-labelledby')!)?.textContent)).toEqual(['This week', 'Outstanding']);
  expect(document.querySelector('.outstanding')?.classList.contains('compact')).toBe(true);
  await fireEvent.click(tabs[0]); await screen.findByText('First item'); expect(h.agenda).toHaveBeenCalledTimes(1);
  await fireEvent.click(screen.getByRole('button', { name: 'Range: Week' })); await fireEvent.click(screen.getByRole('menuitemradio', { name: 'Day' }));
  expect(localStorage.getItem('pad-agenda-mode:user-a')).toBe('day'); expect(screen.getByRole('button', { name: 'Range: Day' })).toBeTruthy();
  expect(screen.queryByRole('tablist', { name: 'Show' })).toBeNull(); expect(document.querySelector('[role="tabpanel"]')).toBeNull();
 });
 it('the phone switch keeps each surface where it was scrolled to', async () => {
  vi.spyOn(window, 'matchMedia').mockImplementation(query => ({ matches: query === '(max-width: 768px)', media: query, addEventListener: vi.fn(), removeEventListener: vi.fn() }) as unknown as MediaQueryList);
  const scrollTo = vi.spyOn(window, 'scrollTo').mockImplementation(() => {});
  const at = (y: number) => Object.defineProperty(window, 'scrollY', { value: y, configurable: true, writable: true });
  try {
   await mount();
   const [schedule, outstanding] = screen.getAllByRole('tab');
   at(420); await fireEvent.click(outstanding);
   await waitFor(() => expect(scrollTo).toHaveBeenLastCalledWith(0, 0));
   at(130); await fireEvent.click(schedule);
   await waitFor(() => expect(scrollTo).toHaveBeenLastCalledWith(0, 420));
   expect(screen.getByText('First item')).toBeTruthy();
   at(55); await fireEvent.click(outstanding);
   await waitFor(() => expect(scrollTo).toHaveBeenLastCalledWith(0, 130));
   expect(scrollTo).toHaveBeenCalledTimes(3);
  } finally { at(0); }
 });
});

describe('week strip, attention band and states', () => {
 const owed = (extra: Partial<AgendaWorkEntry> = {}) => workRow({ item_ref: 'EX-40', item_title: 'Owed item', schedule_id: 'schedule-4', at: '2026-11-03T14:00:00Z', day: '2026-11-03', wall: '2026-11-03T09:00:00', overdue: true, reason: 'target_passed', roles: { ...row().roles!, target: '2026-11-03T14:00:00Z' }, ...extra });
 it('the strip runs seven days from the selected day and jumps to a day heading', async () => {
  await mount();
  expect(stripDays().map(day => day.dataset.stripDay)).toEqual(['2026-11-05', '2026-11-06', '2026-11-07', '2026-11-08', '2026-11-09', '2026-11-10', '2026-11-11']);
  expect(stripDay('2026-11-05')).toHaveAttribute('aria-current', 'date'); expect(stripDay('2026-11-05').querySelectorAll('.pip')).toHaveLength(1);
  expect(sections()).toHaveLength(7);
  await fireEvent.click(stripDay('2026-11-08'));
  await waitFor(() => expect(document.activeElement).toBe(within(daySection('2026-11-08')).getByRole('heading', { name: 'Sunday 8 November' })));
  expect(h.agenda).toHaveBeenCalledTimes(1);
 });
 it('in Day range the strip stays put and selects the day', async () => {
  await mount(); await fireEvent.click(screen.getByRole('button', { name: 'Day' }));
  await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(2));
  expect(stripDay('2026-11-05')).toHaveAttribute('aria-pressed', 'true');
  await fireEvent.click(stripDay('2026-11-07'));
  await waitFor(() => expect(h.agenda.mock.lastCall?.slice(0, 2)).toEqual(['2026-11-07T05:00:00.000Z', '2026-11-08T05:00:00.000Z']));
  expect(stripDays()[0].dataset.stripDay).toBe('2026-11-05'); expect(stripDay('2026-11-07')).toHaveAttribute('aria-pressed', 'true');
  expect(screen.getByRole('heading', { name: 'November 7, 2026' })).toBeTruthy();
 });
 it('a drag turns the strip into drop zones, previews once per day and moves on a strip drop', async () => {
  await mount(); expect(stripDays().every(day => day.dataset.drop === undefined)).toBe(true);
  await fireEvent.dragStart(card());
  expect(stripDays().every(day => day.dataset.drop === 'true')).toBe(true); expect(card().classList.contains('dragging')).toBe(true);
  await fireEvent.dragEnter(stripDay()); await fireEvent.dragEnter(daySection()); await fireEvent.dragEnter(stripDay('2026-11-07')); await fireEvent.dragEnter(stripDay());
  expect(h.preview).toHaveBeenCalledTimes(2);
  expect(h.preview.mock.calls.map(call => call[2].to)).toEqual(['2026-11-06T14:00:00.000Z', '2026-11-07T14:00:00.000Z']);
  expect(within(stripDay()).getByText('Move here')).toBeTruthy();
  await waitFor(() => expect(document.querySelector('.dragbar')?.textContent).toMatch(/Moving “First item” to Fri 6, 09:00\..*Esc/));
  expect(daySection().querySelector('li.ghost')?.textContent).toMatch(/09:00.*First item.*Moves here/);
  await fireEvent.drop(stripDay());
  await waitFor(() => expect(h.move).toHaveBeenCalledWith('work', 'EX-30', { role: 'target', to: '2026-11-06T14:00:00.000Z', revision: 'revision-1' }));
  expect(h.preview).toHaveBeenCalledTimes(2); expect(document.querySelector('.dragbar')).toBeNull();
 });
 it('the drag bar says what follows and what the deadline does', async () => {
  const roles = { ...row().roles!, target: row().at, deadline: '2026-11-09T14:00:00Z' };
  h.agenda.mockResolvedValue(response([row({ roles, roles_origin: 'native' })]));
  h.preview.mockImplementation((_ws, _ref, body) => Promise.resolve(preview(body.to, { changes: [...preview(body.to).changes, { item_id: 'linked', role: 'target', to: '2026-11-08T14:00:00Z' }] })));
  await mount(); await drag();
  await waitFor(() => expect(document.querySelector('.dragbar')?.textContent).toMatch(/1 linked item follows · deadline Mon 9, 09:00 unchanged/));
 });
 it('an occurrence that cannot move does not start a drag', async () => {
  h.agenda.mockResolvedValue(response([row({ roles: { ...row().roles!, deadline: '2026-11-06T14:00:00Z' }, roles_origin: 'legacy_backfill' })]));
  await mount(); const item = document.querySelector('li[data-ref="EX-30"]')!;
  expect(item.getAttribute('draggable')).not.toBe('true');
  const start = new Event('dragstart', { bubbles: true, cancelable: true }); item.dispatchEvent(start); flushSync();
  expect(start.defaultPrevented).toBe(true); expect(stripDays().every(day => day.dataset.drop === undefined)).toBe(true);
 });
 it('lists what needs attention once, above the days, and not again in its day', async () => {
  const late = row({ item_ref: 'EX-41', item_title: 'Late item', schedule_id: 'schedule-5', overdue: true });
  const lapsed = row({ item_ref: 'EX-42', item_title: 'Lapsed item', schedule_id: 'schedule-6', state: 'lapsed', at: '2026-11-05T10:00:00Z', wall: '2026-11-05T05:00:00' });
  h.agenda.mockResolvedValue(response([row(), late, lapsed], { priority_list: [owed(), workRow({ ...late, reason: 'target_passed' }), workRow()] }));
  await mount();
  const band = screen.getByRole('region', { name: 'Needs attention' });
  expect([...band.querySelectorAll('li')].map(li => li.getAttribute('data-ref'))).toEqual(['EX-40', 'EX-41', 'EX-42']);
  expect(within(band).getByText('3')).toBeTruthy(); expect(band.querySelector('[draggable="true"]')).toBeNull();
  expect(band.querySelector('li[data-ref="EX-40"] .time')?.textContent).toMatch(/Tue 3.*09:00/);
  expect([...daySection('2026-11-05').querySelectorAll('li[data-ref]')].map(li => li.getAttribute('data-ref'))).toEqual(['EX-30']);
  expect(within(daySection('2026-11-05')).getByText('2 owed · 1 missed')).toBeTruthy();
  expect(stripDay('2026-11-05').querySelectorAll('.pip')).toHaveLength(3);
  h.view.mockResolvedValue({ next_occurrence: { at: '2026-11-03T14:00:00Z', wall: '2026-11-03T09:00:00' } });
  await fireEvent.click(within(band).getByRole('button', { name: 'Done EX-40' }));
  await waitFor(() => expect(h.resolve).toHaveBeenCalledWith('work', 'EX-40', expect.objectContaining({ state: 'done' })));
 });
 it('says where a day went when its only entry is listed under Needs attention', async () => {
  const lapsed = row({ item_ref: 'EX-42', item_title: 'Lapsed item', schedule_id: 'schedule-6', state: 'lapsed', at: '2026-11-06T10:00:00Z', day: '2026-11-06', wall: '2026-11-06T05:00:00' });
  h.agenda.mockResolvedValue(response([row(), lapsed])); await mount();
  const day = daySection('2026-11-06');
  expect(within(screen.getByRole('region', { name: 'Needs attention' })).getByText('Lapsed item')).toBeTruthy();
  expect(within(day).getByText('1 missed')).toBeTruthy(); expect(day.querySelector('li[data-ref]')).toBeNull();
  expect(within(day).getByText('Listed under Needs attention.')).toBeTruthy(); expect(within(day).queryByText('Nothing scheduled.')).toBeNull();
  expect(within(daySection('2026-11-07')).getByText('Nothing scheduled.')).toBeTruthy();
 });
 it('has no attention band when nothing needs it', async () => {
  await mount(); expect(screen.queryByRole('region', { name: 'Needs attention' })).toBeNull();
 });
 it('Mark done records a lapsed occurrence at its own instant, without reading the next one', async () => {
  const lapsed = row({ item_ref: 'EX-42', item_title: 'Lapsed item', schedule_id: 'schedule-6', state: 'lapsed', at: '2026-11-05T10:00:00Z', wall: '2026-11-05T05:00:00' });
  h.agenda.mockResolvedValue(response([row(), lapsed])); await mount();
  await fireEvent.click(screen.getByRole('button', { name: 'Mark done EX-42' }));
  await waitFor(() => expect(h.resolve).toHaveBeenCalledWith('work', 'EX-42', { occurrence_at: '2026-11-05T10:00:00Z', state: 'done' }));
  expect(h.view).not.toHaveBeenCalled(); await said('Occurrence marked done.', 'success'); expect(h.agenda).toHaveBeenCalledTimes(2);
  h.resolve.mockRejectedValueOnce({ code: 'stale' }); await fireEvent.click(screen.getByRole('button', { name: 'Mark done EX-42' }));
  await said(staleCopy); await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(3));
 });
 it('shows a skeleton while loading, then one error card with one retry and a way back', async () => {
  const first = deferred<AgendaResponse>(); h.agenda.mockReturnValueOnce(first.promise); render(Agenda);
  await waitFor(() => expect(document.querySelector('.main[aria-busy="true"]')).toBeTruthy());
  expect(document.querySelectorAll('.skeleton')).toHaveLength(3); expect(stripDays()).toHaveLength(7); expect(sections()).toHaveLength(0);
  first.resolve(response()); await screen.findByText('First item'); expect(document.querySelector('.skeleton')).toBeNull();
  h.agenda.mockRejectedValue(new Error('offline')); await fireEvent.click(screen.getByRole('button', { name: 'Next week' }));
  await screen.findByText('Could not load this week');
  expect(screen.getAllByRole('button', { name: 'Try again' })).toHaveLength(1); expect(sections()).toHaveLength(0); expect(screen.queryByText(/Nothing scheduled/)).toBeNull();
  h.agenda.mockResolvedValue(response()); await fireEvent.click(screen.getByRole('button', { name: 'Back to this week' }));
  await screen.findByText('First item'); expect(screen.queryByText('Could not load this week')).toBeNull();
  expect(h.agenda.mock.lastCall?.[0]).toBe('2026-11-05T05:00:00.000Z');
 });
 it('offers no way back when the failed week is this week', async () => {
  h.agenda.mockRejectedValueOnce(new Error('offline')); render(Agenda); await screen.findByText('Could not load this week');
  expect(screen.queryByRole('button', { name: 'Back to this week' })).toBeNull();
  h.agenda.mockResolvedValue(response()); await fireEvent.click(screen.getByRole('button', { name: 'Try again' })); await screen.findByText('First item');
 });
 it('Load the rest asks from next_from and joins the two answers', async () => {
  h.agenda.mockResolvedValueOnce(response([row()], { truncated: true, window_complete: false, next_from: '2026-11-08T17:00:00Z', priority_list: [workRow()] }))
   .mockResolvedValueOnce(response([row(), row({ item_ref: 'EX-31', item_title: 'Later item', schedule_id: 's2', at: '2026-11-09T14:00:00Z', day: '2026-11-09', wall: '2026-11-09T09:00:00' })], { priority_list: [] }));
  await mount(); expect(screen.getByText('Showing entries up to Sun 8 November.')).toBeTruthy();
  expect(within(daySection('2026-11-07')).getByText('Nothing scheduled.')).toBeTruthy(); expect(within(daySection('2026-11-09')).queryByText('Nothing scheduled.')).toBeNull();
  await fireEvent.click(screen.getByRole('button', { name: 'Load the rest' }));
  await screen.findByText('Later item');
  expect(h.agenda.mock.calls[1]).toEqual(['2026-11-08T17:00:00Z', '2026-11-12T05:00:00.000Z', 'America/New_York', 'all', ['completed']]);
  expect(screen.getAllByText('First item')).toHaveLength(2); expect(document.querySelectorAll('section[data-day] li[data-ref="EX-30"]')).toHaveLength(1);
  expect(screen.queryByText(/Showing entries up to/)).toBeNull(); expect(screen.queryByRole('button', { name: 'Load the rest' })).toBeNull();
  expect(within(screen.getByRole('region', { name: 'Outstanding' })).getByText('First item')).toBeTruthy();
 });
 it('Load the rest keeps the notice when the rest fails or the identity changed', async () => {
  h.agenda.mockResolvedValueOnce(response([row()], { truncated: true, next_from: '2026-11-08T17:00:00Z' })).mockRejectedValueOnce(new Error('offline'));
  await mount(); await fireEvent.click(screen.getByRole('button', { name: 'Load the rest' }));
  await said('Could not load the rest. Try again.'); expect(screen.getByRole('button', { name: 'Load the rest' })).toBeEnabled(); expect(screen.getByText('First item')).toBeTruthy();
  const rest = deferred<AgendaResponse>(); h.agenda.mockReturnValueOnce(rest.promise); await fireEvent.click(screen.getByRole('button', { name: 'Load the rest' }));
  h.epoch++; rest.resolve(response([row({ item_ref: 'EX-31', item_title: 'Later item', schedule_id: 's2' })])); await settle();
  expect(screen.queryByText('Later item')).toBeNull();
 });
 it('offers no Load the rest when next_from is the end of the window', async () => {
  h.agenda.mockResolvedValue(response([row()], { truncated: true, next_from: '2026-11-12T05:00:00Z', unexpanded: [{ schedule_id: 's9', item_ref: 'EX-90', item_title: 'Dense', collection: 'tasks', reason: 'scan_limit' }] }));
  await mount();
  expect(screen.getByText('Too much to load at once. Choose a smaller range or a workspace group.')).toBeTruthy();
  expect(screen.queryByRole('button', { name: 'Load the rest' })).toBeNull(); expect(screen.queryByText(/Showing entries up to/)).toBeNull();
  expect(h.agenda).toHaveBeenCalledTimes(1);
  cleanup(); h.agenda.mockResolvedValue(response([row()], { next_from: '2026-11-12T00:00:00-05:00' }));
  await mount();
  expect(screen.getByText('Some schedules could not be shown.')).toBeTruthy(); expect(screen.queryByRole('button', { name: 'Load the rest' })).toBeNull();
 });
 it('a month read to its end marks no tail and hides nothing', async () => {
  localStorage.setItem('pad-agenda-mode:user-a', 'month');
  h.agenda.mockResolvedValue(response([row(), row({ item_ref: 'EX-31', item_title: 'Late item', schedule_id: 's2', at: '2026-11-30T14:00:00Z', day: '2026-11-30', wall: '2026-11-30T09:00:00' })], { next_from: '2026-12-01T05:00:00Z' }));
  render(Agenda); await screen.findByText('Late item');
  expect(screen.queryByRole('button', { name: 'Load the rest' })).toBeNull(); expect(screen.queryByText(/Showing entries up to/)).toBeNull();
 });
 it('in Day range the strip claims nothing about the days it did not load', async () => {
  await mount(); await fireEvent.click(screen.getByRole('button', { name: 'Day' }));
  await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(2)); await screen.findAllByText('First item');
  expect(stripDay('2026-11-05')).toHaveAccessibleName('Thursday 5 November, Today, 1 entry, 1 owed'); expect(stripDay('2026-11-05').querySelectorAll('.pip')).toHaveLength(1);
  for (const day of stripDays().slice(1)) {
   expect(day).toHaveAccessibleName(/, not loaded$/); expect(day.getAttribute('aria-label')).not.toMatch(/entr/);
   expect(day.querySelectorAll('.pip')).toHaveLength(0); expect(day.querySelector('.n')?.textContent).toBe('');
  }
  expect(stripDay('2026-11-06')).toHaveAccessibleName('Friday 6 November, not loaded');
  h.agenda.mockResolvedValue(response([row({ at: '2026-11-07T14:00:00Z', day: '2026-11-07', wall: '2026-11-07T09:00:00' })]));
  await fireEvent.click(stripDay('2026-11-07'));
  await waitFor(() => expect(stripDay('2026-11-07')).toHaveAccessibleName('Saturday 7 November, 1 entry, 1 owed'));
  expect(stripDay('2026-11-05')).toHaveAccessibleName('Thursday 5 November, Today, not loaded'); expect(stripDay('2026-11-05').querySelectorAll('.pip')).toHaveLength(0);
 });
 it('in Week range the strip counts only the days before next_from', async () => {
  h.agenda.mockResolvedValue(response([row(), row({ item_ref: 'EX-31', item_title: 'Cut day item', schedule_id: 's2', at: '2026-11-08T14:00:00Z', day: '2026-11-08', wall: '2026-11-08T09:00:00' }),
   row({ type: 'date', item_ref: 'EX-32', item_title: 'Dated later', schedule_id: undefined, at: '2026-11-10T05:00:00Z', day: '2026-11-10' })], { truncated: true, next_from: '2026-11-08T17:00:00Z' }));
  await mount();
  expect(stripDay('2026-11-05')).toHaveAccessibleName('Thursday 5 November, Today, 1 entry, 1 owed');
  expect(stripDay('2026-11-07')).toHaveAccessibleName('Saturday 7 November, no entries');
  expect(stripDay('2026-11-08')).toHaveAccessibleName('Sunday 8 November, not fully loaded'); expect(stripDay('2026-11-08').querySelectorAll('.pip')).toHaveLength(1);
  for (const day of ['2026-11-09', '2026-11-10', '2026-11-11']) {
   expect(stripDay(day)).toHaveAccessibleName(/, not loaded$/); expect(stripDay(day).querySelectorAll('.pip')).toHaveLength(0); expect(stripDay(day).querySelector('.n')?.textContent).toBe('');
  }
 });
 it('rows from Load the rest stay after an action on one of them', async () => {
  const later = row({ item_ref: 'EX-31', item_title: 'Later item', schedule_id: 's2', at: '2026-11-09T14:00:00Z', day: '2026-11-09', wall: '2026-11-09T09:00:00' });
  const first = () => response([row()], { truncated: true, window_complete: false, next_from: '2026-11-08T17:00:00Z' });
  h.agenda.mockResolvedValueOnce(first()).mockResolvedValueOnce(response([later]));
  await mount(); await fireEvent.click(screen.getByRole('button', { name: 'Load the rest' })); await screen.findByText('Later item');
  const second = deferred<AgendaResponse>(); h.agenda.mockResolvedValueOnce(first()).mockReturnValueOnce(second.promise);
  h.view.mockResolvedValueOnce({ item_ref: 'EX-31', next_occurrence: { at: later.at, wall: later.wall } });
  await fireEvent.click(screen.getByRole('button', { name: 'Done EX-31' }));
  await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(4));
  // Nothing is published between the two pages, so the second page never flickers out.
  await settle(); expect(screen.getByText('Later item')).toBeTruthy(); expect(screen.queryByText(/Showing entries up to/)).toBeNull();
  second.resolve(response([later])); await said('Occurrence marked done.', 'success');
  expect(h.agenda.mock.calls[3]).toEqual(['2026-11-08T17:00:00Z', '2026-11-12T05:00:00.000Z', 'America/New_York', 'all', ['completed']]);
  expect(screen.getByText('Later item')).toBeTruthy(); expect(screen.getAllByText('First item').length).toBeGreaterThan(0);
  expect(screen.queryByText(/Showing entries up to/)).toBeNull(); expect(screen.queryByRole('button', { name: 'Load the rest' })).toBeNull();
  expect(document.querySelectorAll('section[data-day] li[data-ref="EX-30"]')).toHaveLength(1);
 });
 it('a refresh whose later page fails keeps the first page and offers the rest again, with no refresh error', async () => {
  const later = row({ item_ref: 'EX-31', item_title: 'Later item', schedule_id: 's2', at: '2026-11-09T14:00:00Z', day: '2026-11-09', wall: '2026-11-09T09:00:00' });
  const first = () => response([row()], { truncated: true, next_from: '2026-11-08T17:00:00Z' });
  h.agenda.mockResolvedValueOnce(first()).mockResolvedValueOnce(response([later]));
  await mount(); await fireEvent.click(screen.getByRole('button', { name: 'Load the rest' })); await screen.findByText('Later item');
  h.agenda.mockResolvedValueOnce(first()).mockRejectedValueOnce(new Error('offline'));
  await fireEvent.click(screen.getByRole('button', { name: 'Done EX-30' })); await said('Occurrence marked done.', 'success');
  expect(h.agenda).toHaveBeenCalledTimes(4); expect(screen.getByText('First item')).toBeTruthy(); expect(screen.queryByText('Later item')).toBeNull();
  expect(screen.getByRole('button', { name: 'Load the rest' })).toBeEnabled(); expect(screen.queryByText('Saved. Could not refresh the view.')).toBeNull();
 });
 it('a new window forgets that the rest was asked for, and a refresh stops paging at ten pages', async () => {
  const first = () => response([row()], { truncated: true, next_from: '2026-11-08T17:00:00Z' });
  h.agenda.mockResolvedValueOnce(first()).mockResolvedValueOnce(response([]));
  await mount(); await fireEvent.click(screen.getByRole('button', { name: 'Load the rest' })); await waitFor(() => expect(screen.queryByRole('button', { name: 'Load the rest' })).toBeNull());
  h.agenda.mockResolvedValue(response([row({ at: '2026-11-12T14:00:00Z', day: '2026-11-12', wall: '2026-11-12T09:00:00' })], { truncated: true, next_from: '2026-11-13T17:00:00Z' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Next week' })); await screen.findByText('Showing entries up to Fri 13 November.');
  await settle(); expect(h.agenda).toHaveBeenCalledTimes(3);
  // Each answer names the same cursor, so only the bound ends the paging.
  await fireEvent.click(screen.getByRole('button', { name: 'Load the rest' })); await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(4));
  h.view.mockResolvedValueOnce({ item_ref: 'EX-30', next_occurrence: { at: '2026-11-12T14:00:00Z', wall: '2026-11-12T09:00:00' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Done EX-30' })); await said('Occurrence marked done.', 'success');
  expect(h.agenda).toHaveBeenCalledTimes(4 + 1 + 10); expect(screen.getByRole('button', { name: 'Load the rest' })).toBeEnabled();
 });
 it('in Day range the strip pages a whole week when the day steps past it', async () => {
  await mount(); await fireEvent.click(screen.getByRole('button', { name: 'Day' }));
  await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(2));
  for (let step = 1; step <= 8; step++) {
   await fireEvent.click(screen.getByRole('button', { name: 'Next day' }));
   await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(2 + step));
   expect(stripDays()[0].dataset.stripDay).toBe(step < 7 ? '2026-11-05' : '2026-11-12');
  }
  expect(stripDays().map(day => day.dataset.stripDay)).toEqual(['2026-11-12', '2026-11-13', '2026-11-14', '2026-11-15', '2026-11-16', '2026-11-17', '2026-11-18']);
  expect(stripDay('2026-11-13')).toHaveAttribute('aria-pressed', 'true');
  for (let step = 1; step <= 9; step++) await fireEvent.click(screen.getByRole('button', { name: 'Previous day' }));
  await waitFor(() => expect(stripDay('2026-11-04')).toHaveAttribute('aria-pressed', 'true'));
  expect(stripDays()[0].dataset.stripDay).toBe('2026-10-29'); expect(stripDays()[6].dataset.stripDay).toBe('2026-11-04');
 });
 it('a saved Day range holds its strip still from the first step', async () => {
  localStorage.setItem('pad-agenda-mode:user-a', 'day'); await mount();
  await fireEvent.click(screen.getByRole('button', { name: 'Next day' }));
  await waitFor(() => expect(stripDay('2026-11-06')).toHaveAttribute('aria-pressed', 'true'));
  expect(stripDays()[0].dataset.stripDay).toBe('2026-11-05');
 });
 it('an error the dialog showed is not repeated as a toast when the dialog closes', async () => {
  h.resolve.mockRejectedValue(new Error('offline')); await mount(); const d = await dialog('Snooze'); await fillTime(d);
  await fireEvent.click(within(d).getByRole('button', { name: 'Snooze' }));
  await within(d).findByText('Could not update this occurrence. Try again.');
  await fireEvent.click(within(d).getByRole('button', { name: 'Cancel' })); await settle();
  expect(screen.queryByRole('dialog')).toBeNull(); expect(h.toast.mock.calls.filter(call => call[1] === 'error')).toEqual([]);
  // The same failure with no dialog open still reaches the toast container.
  await fireEvent.click(screen.getByRole('button', { name: 'Done EX-30' })); await said('Could not update this occurrence. Try again.');
 });
 it('one clock moves a deadline pill from soon to passed on an open page, and stops with the page', async () => {
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
  try {
   h.agenda.mockResolvedValue(response([row({ roles: { ...row().roles!, deadline: '2026-11-05T12:30:00Z', deadline_behavior: 'stays_owed' }, roles_origin: 'native' })]));
   await mount(); const pill = () => document.querySelector('li[data-ref="EX-30"] [data-kind="deadline"]');
   expect(pill()).toHaveAttribute('data-tone', 'soon'); expect(vi.getTimerCount()).toBe(1);
   vi.mocked(Date.now).mockReturnValue(Date.parse('2026-11-05T12:31:00Z')); vi.advanceTimersByTime(60_000); flushSync();
   expect(pill()).toHaveAttribute('data-tone', 'passed'); expect(pill()?.textContent).toContain('Deadline passed 1 min ago · still owed');
   expect(h.agenda).toHaveBeenCalledTimes(1);
   cleanup(); expect(vi.getTimerCount()).toBe(0);
  } finally { vi.useRealTimers(); }
 });
 it('names the week on a phone without the year', async () => {
  vi.spyOn(window, 'matchMedia').mockImplementation(query => ({ matches: query === '(max-width: 768px)', media: query, addEventListener: vi.fn(), removeEventListener: vi.fn() }) as unknown as MediaQueryList);
  await mount(); expect(screen.getByRole('heading', { name: '5–11 Nov' })).toBeTruthy();
 });
 it('names the day in short form on a phone and in full on a desktop', async () => {
  localStorage.setItem('pad-agenda-mode:user-a', 'day');
  await mount(); expect(screen.getByRole('heading', { name: 'November 5, 2026' })).toBeTruthy();
  cleanup();
  vi.spyOn(window, 'matchMedia').mockImplementation(query => ({ matches: query === '(max-width: 768px)', media: query, addEventListener: vi.fn(), removeEventListener: vi.fn() }) as unknown as MediaQueryList);
  await mount(); expect(screen.getByRole('heading', { name: 'Thu 5 Nov' })).toBeTruthy(); expect(screen.queryByRole('heading', { name: 'November 5, 2026' })).toBeNull();
 });
});

describe('target move failure and drag fences', () => {
 it.each([
  ['forbidden', 'You can no longer change this item.', true],
  ['permission_denied', 'You can no longer change this item.', true],
  ['link_edit_required', 'You can no longer change this item.', true],
  ['stale', 'This occurrence changed. Review the updated entry and try again.', true],
  ['deadline_approval_required', 'Server explanation', false],
  ['deadline_owner_required', 'Server explanation', false],
  ['schedule_linked', 'Server explanation', false],
  ['internal', 'Could not update this occurrence. Try again.', false]
 ])('handles %s without reissuing the move', async (code, copy, reload) => {
  h.move.mockRejectedValue({ code, message: 'Server explanation' }); await mount(); await drop(); await said(copy);
  expect(h.move).toHaveBeenCalledTimes(1); expect(h.preview).toHaveBeenCalledTimes(1);
  expect(h.agenda).toHaveBeenCalledTimes(reload ? 2 : 1);
 });
 it('keeps saved rows disabled when refresh fails and retries only the read', async () => {
  await mount(); h.agenda.mockRejectedValueOnce(new Error('offline')); await drop();
  await screen.findByText('Saved. Could not refresh the view.');
  expect(screen.getByRole('button', { name: 'Actions EX-30' })).toBeDisabled();
  h.agenda.mockResolvedValue(response()); await fireEvent.click(screen.getByRole('button', { name: 'Try again' })); await settle(); expect(h.move).toHaveBeenCalledTimes(1);
 });
 it('returns focus to the period heading when the moved card leaves the view', async () => {
  await mount(); h.agenda.mockResolvedValue(response([])); await drop();
  await waitFor(() => expect(h.toast.mock.calls.some(call => /^Occurrence moved to/.test(call[0]) && call[1] === 'success')).toBe(true));
  expect(document.activeElement).toBe(screen.getByRole('heading', { name: '5–11 November 2026' }));
 });
 it('compares the root source to roles.target as an instant, with different RFC3339 offsets', async () => {
  h.agenda.mockResolvedValue(response([row({ roles: { ...row().roles!, target: '2026-11-05T15:00:00Z' } })]));
  h.preview.mockResolvedValue(preview(undefined, { changes: [{ ...preview().changes[0], from: '2026-11-05T10:00:00-05:00' }] }));
  await mount(); await drop(); await waitFor(() => expect(h.move).toHaveBeenCalledTimes(1));
 });
 it('does not offer Bring to today on today and does not drag history or legacy hard deadlines', async () => {
  h.agenda.mockResolvedValue(response([row(), row({ item_ref: 'EX-31', item_title: 'Lapsed item', state: 'lapsed' }), row({ item_ref: 'EX-32', item_title: 'Legacy item', roles: { ...row().roles!, deadline: '2026-11-06T14:00:00Z' }, roles_origin: 'legacy_backfill' })]));
  await mount(); await menu(); expect(screen.queryByRole('menuitem', { name: 'Bring to today' })).toBeNull();
  expect(card('EX-31')).toBeNull(); expect(card('EX-32')).toBeNull();
 });
 it('keeps a pending preview from committing after identity changes', async () => {
  const p = deferred<CommitmentMoveResult>(); h.preview.mockReturnValue(p.promise); await mount(); await drop();
  h.epoch++; p.resolve(preview()); await settle(); expect(h.move).not.toHaveBeenCalled();
 });
 it('clears dragend and ignores late ghost previews; a new drag gets a fresh preview', async () => {
  const p = deferred<CommitmentMoveResult>(); h.preview.mockReturnValueOnce(p.promise); await mount(); const source = card(); await drag();
  await fireEvent.dragEnd(source); p.resolve(preview(undefined, { changes: [...preview().changes, { item_id: 'linked', role: 'target', to: '2026-11-07T14:00:00Z' }] }));
  await settle(); expect(screen.queryByText(/proposed/)).toBeNull(); expect(h.move).not.toHaveBeenCalled();
  await drop(); await waitFor(() => expect(h.move).toHaveBeenCalledTimes(1)); expect(h.preview).toHaveBeenCalledTimes(2);
 });
});

describe('forecast actions', () => {
 const milestone={children:[],counted:0,total:0,waivers:0,result:null};
 it('opens an empty forecast from Day actions and quietly reloads after saving',async()=>{
  localStorage.setItem('pad-agenda-mode:user-a','day');
  h.view.mockResolvedValue({forecast:null,milestone,roles:{target:'2026-11-05'}});
  h.setForecast.mockResolvedValue({});
  await mount();await menu();await fireEvent.click(screen.getByRole('menuitem',{name:'Set forecast…'}));
  const d=await screen.findByRole('dialog',{name:'Forecast'});await waitFor(()=>expect(within(d).getByLabelText('New forecast date')).not.toBeDisabled());
  expect(d.querySelector('.subject')).toHaveTextContent('First item · First workspace EX-30');
  expect((within(d).getByLabelText('New forecast date') as HTMLInputElement).value).toBe('');
  await fireEvent.input(within(d).getByLabelText('New forecast date'),{target:{value:'2026-11-10'}});
  await fireEvent.click(within(d).getByRole('button',{name:'Save forecast'}));
  await waitFor(()=>expect(h.setForecast).toHaveBeenCalledWith('work','EX-30',{value:'2026-11-10'}));
  await waitFor(()=>expect(h.agenda).toHaveBeenCalledTimes(2));
  expect(screen.queryByText('Loading agenda…')).toBeNull();
 });
 it('opens Forecast from the priority list and clears the estimate',async()=>{
  h.agenda.mockResolvedValue(response([row()],{priority_list:[workRow({roles:{...row().roles!,target:row().at},forecast:{value:'2026-11-10',set_by:'u1',set_by_kind:'agent',set_at:'now',source:''}})]}));
  h.view.mockResolvedValue({forecast:{value:'2026-11-10',set_by:'u1',set_by_kind:'agent',set_at:'now',source:'',passed:false},milestone});
  h.clearForecast.mockResolvedValue({});
  render(Agenda);await fireEvent.click(await screen.findByRole('button',{name:'Actions for EX-30'}));await fireEvent.click(screen.getByRole('menuitem',{name:'Forecast…'}));
  const d=await screen.findByRole('dialog',{name:'Forecast'});await within(d).findByRole('button',{name:'Clear forecast'});
  await fireEvent.click(within(d).getByRole('button',{name:'Clear forecast'}));
  await waitFor(()=>expect(h.clearForecast).toHaveBeenCalledWith('work','EX-30'));
  await waitFor(()=>expect(h.agenda).toHaveBeenCalledTimes(2));
 });
});


describe('workspace groups and daily counts', () => {
 const choose = async (name: string) => { await fireEvent.click(filter()); await fireEvent.click(groupItem(name)); };
 const manage = async () => { await fireEvent.click(filter()); await fireEvent.click(screen.getByRole('menuitem', { name: 'Manage groups…' })); return screen.getByRole('dialog', { name: 'Workspace groups' }); };
 const listed = async () => { await fireEvent.click(filter()); const names = groupItems(); await fireEvent.click(filter()); return names; };
 it('lists groups in server order, selects and remembers a group', async () => {
  h.groups.mockResolvedValue({ groups: [...workspaceGroups, { id: 'group-2', name: 'Another', kind: 'named', workspace_ids: ['ws-1'] }] });
  await mount();
  expect(filter()).toHaveTextContent('All workspaces');
  expect(await listed()).toEqual(['All workspaces', 'My workspaces', 'Team', 'Another']);
  await choose('Team');
  await waitFor(() => expect(h.agenda.mock.calls.at(-1)?.[3]).toBe('group-1'));
  expect(filter()).toHaveTextContent('Team'); expect(screen.queryByRole('menu')).toBeNull();
  await fireEvent.click(filter()); expect(groupItem('Team')).toHaveAttribute('aria-checked', 'true'); expect(groupItem('All workspaces')).toHaveAttribute('aria-checked', 'false');
  expect(localStorage.getItem('pad-agenda-group:user-a')).toBe('group-1');
 });
 it.each(['group-1', 'deleted'])('restores a saved group or falls back to all: %s', async saved => {
  localStorage.setItem('pad-agenda-group:user-a', saved); await mount();
  expect(filter()).toHaveTextContent(saved === 'deleted' ? 'All workspaces' : 'Team');
  expect(h.agenda.mock.calls[0][3]).toBe(saved === 'deleted' ? 'all' : saved);
  if (saved === 'deleted') {
   await choose('My workspaces'); expect(localStorage.getItem('pad-agenda-group:user-a')).toBe('mine');
  }
 });
 it('recovers from a saved named group deleted elsewhere without a load error', async () => {
  localStorage.setItem('pad-agenda-group:user-a', 'group-1');
  h.agenda.mockRejectedValueOnce({ code: 'not_found' }).mockResolvedValue(response());
  h.groups.mockResolvedValueOnce({ groups: workspaceGroups }).mockResolvedValue({ groups: workspaceGroups.slice(0, 2) });
  await mount();
  expect(h.agenda.mock.calls.map(call => call[3])).toEqual(['group-1', 'all']);
  expect(h.groups).toHaveBeenCalledTimes(2); expect(filter()).toHaveTextContent('All workspaces');
  expect(screen.queryByText(/Could not load/)).toBeNull();
 });
 it('loads the original agenda without a group when the groups list fails', async () => {
  h.groups.mockRejectedValue(new Error('Unavailable')); await mount();
  expect(screen.queryByRole('button', { name: /^Workspaces: / })).toBeNull();
  expect(h.agenda.mock.calls[0].slice(3)).toEqual([undefined, ['completed']]); expect(screen.queryByRole('alert')).toBeNull(); expect(h.toast).not.toHaveBeenCalled();
 });
 it.each([{ ids: [] }, { ids: ['gone'] }])('renders an empty named group with no accessible workspaces: %j', async ({ ids }) => {
  h.groups.mockResolvedValue({ groups: [...workspaceGroups.slice(0, 2), { ...workspaceGroups[2], workspace_ids: ids }] });
  h.agenda.mockResolvedValue(response([], { counts: [{ day: '2026-11-05', completed: 1, open: 2, burndown: 2 }] }));
  render(Agenda); await screen.findByRole('button', { name: /^Workspaces: / }); await choose('Team');
  await screen.findByText('No workspaces in this group.');
  expect(screen.queryByText(/Could not load/)).toBeNull(); expect(document.querySelector('.layout')).toBeNull(); expect(stripDays()).toHaveLength(0);
  expect(screen.queryByRole('table', { name: 'Daily counts' })).toBeNull(); expect(document.querySelector('[data-stat]')).toBeNull();
  await fireEvent.click(screen.getByRole('button', { name: 'Add workspaces' }));
  const d = screen.getByRole('dialog', { name: 'Workspace groups' });
  expect(within(d).getByRole('textbox', { name: 'Name' })).toHaveValue('Team'); expect(within(d).getByRole('button', { name: 'Save group' })).toBeTruthy();
 });
 it('renders normal surfaces for a populated group with no entries', async () => {
  h.agenda.mockResolvedValue(response([])); render(Agenda);
  await screen.findByRole('button', { name: /^Workspaces: / }); await choose('Team');
  await waitFor(() => expect(h.agenda.mock.calls.at(-1)?.[3]).toBe('group-1'));
  expect(screen.queryByText('No workspaces in this group.')).toBeNull(); expect(document.querySelector('.layout')).toBeTruthy();
  expect(await screen.findByText('Nothing scheduled for 5–11 November 2026')).toBeTruthy(); expect(screen.getByText('No commitments or dated items in “Team” this week.')).toBeTruthy();
  await fireEvent.click(screen.getByRole('button', { name: 'Show all workspaces' })); await waitFor(() => expect(h.agenda.mock.calls.at(-1)?.[3]).toBe('all'));
  expect(screen.queryByRole('button', { name: 'Show all workspaces' })).toBeNull(); expect(screen.getByText('No commitments or dated items this week.')).toBeTruthy();
 });
 it('skips the empty-group check when workspaces fail to load', async () => {
  h.workspaces.mockRejectedValue(new Error('Unavailable')); await mount(); await choose('Team');
  expect(screen.queryByText('No workspaces in this group.')).toBeNull(); expect(document.querySelector('.layout')).toBeTruthy();
 });
 it('creates a trimmed group with a workspace and refreshes the picker', async () => {
  await mount(); const d = await manage();
  expect(within(d).queryByText('All')).toBeNull(); expect(within(d).queryByText('Mine')).toBeNull();
  await fireEvent.click(within(d).getByRole('button', { name: 'New group' }));
  await fireEvent.input(within(d).getByRole('textbox', { name: 'Name' }), { target: { value: '  New team  ' } });
  await fireEvent.click(within(d).getByRole('checkbox', { name: 'First workspace' }));
  h.groups.mockResolvedValue({ groups: [...workspaceGroups, { id: 'new', name: 'New team', kind: 'named', workspace_ids: ['ws-1'] }] });
  await fireEvent.click(within(d).getByRole('button', { name: 'Create group' }));
  await waitFor(() => expect(h.createGroup).toHaveBeenCalledWith({ name: 'New team', workspace_ids: ['ws-1'] }));
  await waitFor(() => expect(h.groups).toHaveBeenCalledTimes(2)); await settle();
  await fireEvent.click(within(d).getByRole('button', { name: 'Done' })); expect(await listed()).toContain('New team');
 });
 it('renames and edits workspaces, disambiguating duplicate workspace names', async () => {
  h.workspaces.mockResolvedValue([workspace, { ...workspace, id: 'ws-2', owner_username: 'bob' }]);
  await mount(); const d = await manage(); await fireEvent.click(within(d).getByRole('button', { name: 'Edit Team' }));
  await fireEvent.input(within(d).getByRole('textbox', { name: 'Name' }), { target: { value: ' Renamed ' } });
  await fireEvent.click(within(d).getByRole('checkbox', { name: 'alice/First workspace' }));
  await fireEvent.click(within(d).getByRole('checkbox', { name: 'bob/First workspace' }));
  await fireEvent.click(within(d).getByRole('button', { name: 'Save group' }));
  await waitFor(() => expect(h.updateGroup).toHaveBeenCalledWith('group-1', { name: 'Renamed', workspace_ids: ['ws-2'] }));
  expect(h.groups).toHaveBeenCalledTimes(2);
 });
 it('renames by name only when the workspace list failed, leaving membership untouched', async () => {
  h.workspaces.mockRejectedValue(new Error('Unavailable'));
  await mount(); const d = await manage(); await fireEvent.click(within(d).getByRole('button', { name: 'Edit Team' }));
  await fireEvent.input(within(d).getByRole('textbox', { name: 'Name' }), { target: { value: 'Renamed' } });
  await fireEvent.click(within(d).getByRole('button', { name: 'Save group' }));
  await waitFor(() => expect(h.updateGroup).toHaveBeenCalledWith('group-1', { name: 'Renamed' }));
 });
 it('requires confirmation to delete the selected group and reloads all', async () => {
  localStorage.setItem('pad-agenda-group:user-a', 'group-1'); await mount(); const d = await manage();
  await fireEvent.click(within(d).getByRole('button', { name: 'Delete Team' })); expect(h.removeGroup).not.toHaveBeenCalled();
  expect(confirmDialog.active).toMatchObject({ title: 'Delete Team?', confirmLabel: 'Delete group', danger: true });
  h.groups.mockResolvedValue({ groups: workspaceGroups.slice(0, 2) });
  confirmDialog.confirm();
  await waitFor(() => expect(h.removeGroup).toHaveBeenCalledWith('group-1'));
  await waitFor(() => expect(h.agenda.mock.calls.at(-1)?.[3]).toBe('all'));
  expect(filter()).toHaveTextContent('All workspaces'); expect(h.groups).toHaveBeenCalledTimes(2);
 });
 it.each([{ message: 'Name is already used.' }, {}])('shows save failures inside the open dialog: %j', async error => {
  h.createGroup.mockRejectedValue(error); await mount(); const d = await manage(); await fireEvent.click(within(d).getByRole('button', { name: 'New group' }));
  await fireEvent.input(within(d).getByRole('textbox', { name: 'Name' }), { target: { value: 'Team' } });
  await fireEvent.click(within(d).getByRole('button', { name: 'Create group' }));
  await within(d).findByText('message' in error ? error.message : 'Could not save the group. Try again.');
  expect(screen.getByRole('dialog', { name: 'Workspace groups' })).toBe(d);
 });
 it('rejects a whitespace-only group name', async () => {
  await mount(); const d = await manage(); await fireEvent.click(within(d).getByRole('button', { name: 'New group' }));
  await fireEvent.input(within(d).getByRole('textbox', { name: 'Name' }), { target: { value: '   ' } });
  await fireEvent.click(within(d).getByRole('button', { name: 'Create group' })); expect(h.createGroup).not.toHaveBeenCalled();
 });
 it('discards group and workspace results after the identity changes', async () => {
  const old = deferred<WorkspaceGroupsResponse>(); const oldWorkspaces = deferred<typeof workspace[]>();
  h.groups.mockReturnValueOnce(old.promise); h.workspaces.mockReturnValueOnce(oldWorkspaces.promise);
  render(Agenda); await waitFor(() => expect(h.groups).toHaveBeenCalledTimes(1));
  h.user = { id: 'user-b', timezone: 'UTC' }; h.epoch++; h.signal?.set('auth', 1);
  await screen.findByText('First item');
  old.resolve({ groups: [...workspaceGroups, { id: 'private', name: 'Private group', kind: 'named', workspace_ids: [] }] }); oldWorkspaces.resolve([]);
  await settle(); expect(await listed()).toEqual(['All workspaces', 'My workspaces', 'Team']);
  expect(localStorage.getItem('pad-agenda-group:user-b')).toBeNull();
 });
 it('loads the agenda without waiting for the workspace list', async () => {
  h.workspaces.mockReturnValue(new Promise(() => {}));
  await mount(); expect(filter()).toBeTruthy();
 });
 it('refetches workspaces with the groups, so members added elsewhere do not read as an empty group', async () => {
  await mount(); await choose('Team');
  h.workspaces.mockResolvedValue([workspace, { ...workspace, id: 'ws-new', name: 'Added elsewhere' }]);
  h.groups.mockResolvedValue({ groups: [...workspaceGroups.slice(0, 2), { ...workspaceGroups[2], workspace_ids: ['ws-new'] }] });
  const d = await manage(); await fireEvent.click(within(d).getByRole('button', { name: 'Edit Team' }));
  await fireEvent.click(within(d).getByRole('button', { name: 'Save group' }));
  await waitFor(() => expect(h.workspaces).toHaveBeenCalledTimes(2)); await settle();
  expect(screen.queryByText('No workspaces in this group.')).toBeNull();
 });
 it('discards a pending group write after identity changes', async () => {
  const write = deferred<object>(); h.createGroup.mockReturnValueOnce(write.promise);
  await mount(); const d = await manage(); await fireEvent.click(within(d).getByRole('button', { name: 'New group' }));
  await fireEvent.input(within(d).getByRole('textbox', { name: 'Name' }), { target: { value: 'Private' } });
  await fireEvent.click(within(d).getByRole('button', { name: 'Create group' }));
  h.user = { id: 'user-b', timezone: 'UTC' }; h.epoch++; h.signal?.set('auth', 1);
  await waitFor(() => expect(h.groups).toHaveBeenCalledTimes(2));
  write.resolve({}); await settle(); expect(h.groups).toHaveBeenCalledTimes(2);
  expect(screen.queryByRole('dialog', { name: 'Workspace groups' })).toBeNull();
 });
 it('shows the counts-unavailable sentence for a truncated window, with the list numbers kept', async () => {
  h.agenda.mockResolvedValue(response([], { counts_truncated: true, priority_list: [workRow()] })); render(Agenda);
  await screen.findByText('Counts unavailable for this window: too much activity to count.');
  expect(screen.queryByRole('table', { name: 'Daily counts' })).toBeNull();
  expect(document.querySelector('[data-stat="outstanding"] b')?.textContent).toBe('1'); expect(document.querySelector('[data-stat="completed"]')).toBeNull();
 });
 it.each([['week', 'This week'], ['day', 'This day'], ['month', 'This month']])('summarises the counts in %s mode', async (mode, title) => {
  localStorage.setItem('pad-agenda-mode:user-a', mode);
  h.agenda.mockResolvedValue(response([], { counts: [{ day: '2026-11-05', completed: 1, open: 2, burndown: 2 }, { day: '2026-11-06', completed: 4, open: 2, burndown: 2 }] })); render(Agenda);
  const summary = await screen.findByRole('region', { name: title });
  // Tomorrow has no bar and adds nothing to the completed figure.
  expect(summary.querySelectorAll('svg rect[data-day]')).toHaveLength(1); expect(summary.querySelector('[data-stat="completed"] b')?.textContent).toBe('1');
  expect(within(summary).getByRole('table', { name: 'Daily counts' }).querySelectorAll('tbody tr')).toHaveLength(2);
  expect(screen.queryByRole('img', { name: 'Daily counts' })).toBeNull();
 });
 it('does not reload the agenda when a group other than the selected one is renamed', async () => {
  await mount(); const calls = h.agenda.mock.calls.length;
  h.groups.mockResolvedValue({ groups: [...workspaceGroups.slice(0, 2), { ...workspaceGroups[2], name: 'Renamed' }] });
  const d = await manage(); await fireEvent.click(within(d).getByRole('button', { name: 'Edit Team' }));
  await fireEvent.input(within(d).getByRole('textbox', { name: 'Name' }), { target: { value: 'Renamed' } });
  await fireEvent.click(within(d).getByRole('button', { name: 'Save group' }));
  await waitFor(() => expect(h.groups).toHaveBeenCalledTimes(2)); await settle();
  expect(h.agenda.mock.calls.length).toBe(calls); expect(screen.getByText('First item')).toBeTruthy();
  await fireEvent.click(within(d).getByRole('button', { name: 'Done' })); expect(await listed()).toContain('Renamed');
 });
 it('reloads the agenda when the selected group changes its workspaces', async () => {
  await mount(); await choose('Team');
  await waitFor(() => expect(h.agenda.mock.calls.at(-1)?.[3]).toBe('group-1')); const calls = h.agenda.mock.calls.length;
  h.workspaces.mockResolvedValue([workspace, { ...workspace, id: 'ws-2', name: 'Second' }]);
  h.groups.mockResolvedValue({ groups: [...workspaceGroups.slice(0, 2), { ...workspaceGroups[2], workspace_ids: ['ws-1', 'ws-2'] }] });
  const d = await manage(); await fireEvent.click(within(d).getByRole('button', { name: 'Edit Team' }));
  await fireEvent.click(within(d).getByRole('button', { name: 'Save group' }));
  await waitFor(() => expect(h.agenda.mock.calls.length).toBe(calls + 1));
 });
});


describe('agenda Month page', () => {
 beforeEach(() => localStorage.setItem('pad-agenda-mode:user-a', 'month'));
 const dateButton = (day: string) => document.querySelector(`button.date[data-day="${day}"]`) as HTMLButtonElement;
 const cellOf = (day: string) => dateButton(day).closest<HTMLElement>('[role="gridcell"]')!;
 const phone = () => vi.spyOn(window, 'matchMedia').mockImplementation(query => ({ matches: query === '(max-width: 768px)', media: query, addEventListener: vi.fn(), removeEventListener: vi.fn() }) as unknown as MediaQueryList);
 // Server order: three outstanding, one outstanding whose target passed (named by the priority list), one lapsed.
 const crowded = () => response([
  // The first is all-day: all-day entries lead a day, but never ahead of what needs attention.
  ...[0, 1, 2].map(i => row({ item_ref: `EX-${30 + i}`, schedule_id: `s${i}`, item_title: `Plain ${i}`, all_day: i === 0 })),
  row({ item_ref: 'EX-40', schedule_id: 's-late', item_title: 'Late target' }),
  row({ item_ref: 'EX-41', schedule_id: 's-lapsed', item_title: 'Lapsed one', state: 'lapsed', at: '2026-11-05T15:00:00Z', occurrence_key: 'k1' })
 ], { priority_list: [workRow({ item_ref: 'EX-40', schedule_id: 's-late', item_title: 'Late target', reason: 'target_passed' })] });
 it('puts attention entries ahead of the three-entry cut, in red, with the day count and what is owed', async () => {
  h.agenda.mockResolvedValue(crowded()); render(Agenda); await screen.findByRole('grid');
  const cell = cellOf('2026-11-05'); const lines = within(cell).getAllByRole('link');
  expect(lines.map(line => line.querySelector('.t')?.textContent)).toEqual(['Late target', 'Lapsed one', 'Plain 0']);
  expect(lines.map(line => line.classList.contains('attention'))).toEqual([true, true, false]);
  expect(lines[1]).toHaveAttribute('title', 'Lapsed one · 10:00 · Lapsed · First workspace');
  expect(within(cell).getByRole('button', { name: '+2 more' })).toBeTruthy();
  expect(cell.querySelector('.count')).toHaveTextContent('5 · 4 owed');
  expect(within(cell).queryByText('Today')).toBeNull(); expect(dateButton('2026-11-05').getAttribute('aria-label')).toMatch(/, Today, 5 entries$/);
  expect(cellOf('2026-11-06').querySelector('.count')).toBeNull();
 });
 it('lists attention entries first in day detail, under a title and summary for the day', async () => {
  h.agenda.mockResolvedValue(crowded()); render(Agenda); await screen.findByRole('grid'); await fireEvent.click(dateButton('2026-11-05'));
  const detail = screen.getByRole('dialog', { name: 'Thursday, 5 November 2026, Today' });
  expect(detail.querySelector('.subject')).toHaveTextContent('5 entries · 4 owed · 1 missed');
  expect([...detail.querySelectorAll('[data-ref]')].map(el => el.getAttribute('data-ref'))).toEqual(['EX-40', 'EX-41', 'EX-30', 'EX-31', 'EX-32']);
  expect(detail.textContent).not.toContain('Time zone');
 });
 it('on a phone lists every entry of a day as a row with its actions, attention first, and folds empty days', async () => {
  phone(); h.agenda.mockResolvedValue(crowded()); render(Agenda);
  const list = await screen.findByRole('list', { name: 'November 2026' });
  expect([...list.children].map(item => item.querySelector('button.day-header')?.getAttribute('data-day') ?? item.textContent?.trim())).toEqual(['Sun 1 – Wed 4 · nothing scheduled', '2026-11-05', 'Fri 6 – Mon 30 · nothing scheduled']);
  expect([...list.querySelectorAll('[data-ref]')].map(el => el.getAttribute('data-ref'))).toEqual(['EX-40', 'EX-41', 'EX-30', 'EX-31', 'EX-32']);
  expect(within(list).queryByRole('button', { name: /more$/ })).toBeNull();
  expect(within(list).getAllByRole('button', { name: /^Done EX-/ })).toHaveLength(4); expect(within(list).getByRole('button', { name: 'Mark done EX-41' })).toBeTruthy();
  await fireEvent.click(within(list).getByRole('button', { name: 'Done EX-30' }));
  await waitFor(() => expect(h.resolve).toHaveBeenCalledWith('work', 'EX-30', { occurrence_at: frozenAt, state: 'done' }));
  expect(screen.queryByRole('dialog')).toBeNull();
 });
 it('selects Month with aria-pressed and requests the local month through the DST fold', async () => {
  localStorage.setItem('pad-agenda-mode:user-a', 'week'); await mount();
  expect(screen.getByRole('button', { name: 'Month' })).toHaveAttribute('aria-pressed', 'false');
  await fireEvent.click(screen.getByRole('button', { name: 'Month' }));
  await waitFor(() => expect(h.agenda).toHaveBeenLastCalledWith('2026-11-01T04:00:00.000Z', '2026-12-01T05:00:00.000Z', 'America/New_York', 'all', ['completed']));
  expect(screen.getByRole('button', { name: 'Month' })).toHaveAttribute('aria-pressed', 'true');
  expect(localStorage.getItem('pad-agenda-mode:user-a')).toBe('month');
 });
 it('restores saved Month and renders workspace previews with their own routes and three-entry limit', async () => {
  h.agenda.mockResolvedValue(response(Array.from({ length: 4 }, (_, i) => row({ item_ref: `EX-${30+i}`, schedule_id: `s${i}`, item_title: `Month item ${i}` }))));
  render(Agenda); const grid = await screen.findByRole('grid', { name: 'November 2026' });
  expect(screen.getByRole('button', { name: 'Month' })).toHaveAttribute('aria-pressed', 'true');
  expect(h.agenda).toHaveBeenLastCalledWith('2026-11-01T04:00:00.000Z', '2026-12-01T05:00:00.000Z', 'America/New_York', 'all', ['completed']);
  const cell = cellOf('2026-11-05');
  expect(within(grid).getByRole('link', { name: /Month item 0/ })).toHaveAttribute('href', '/alice/work/tasks/EX-30');
  expect(cell.querySelector('.count')).toHaveTextContent('4 · 4 owed');
  expect(within(cell).getAllByRole('link')).toHaveLength(3);
  expect(screen.queryByText('Month item 3')).toBeNull();
  expect(document.querySelector('[draggable="true"]')).toBeNull();
  expect(screen.getAllByRole('status')).toHaveLength(1);
  await fireEvent.click(within(cell).getByRole('button', { name: '+1 more' }));
  expect(within(screen.getByRole('dialog')).getAllByRole('button', { name: /^Done / })).toHaveLength(4);
 });
 it('opens a day and resolves Done in place with the existing commitment identity and announcement', async () => {
  await mount(); await fireEvent.click(dateButton('2026-11-05'));
  const detail = screen.getByRole('dialog');
  h.agenda.mockResolvedValue(response([row({ state: 'done' })]));
  await fireEvent.click(within(detail).getByRole('button', { name: 'Done EX-30' }));
  await waitFor(() => expect(h.resolve).toHaveBeenCalledWith('work', 'EX-30', { occurrence_at: frozenAt, state: 'done' }));
  await said('Occurrence marked done.', 'success');
  expect(screen.getByRole('dialog')).toBe(detail);
  expect(within(detail).queryByRole('button', { name: 'Done EX-30' })).toBeNull();
  expect(within(detail).getByRole('img', { name: 'Done' })).toBeTruthy();
 });
 it('shows a failed Done inside day detail, not behind it', async () => {
  await mount(); await fireEvent.click(dateButton('2026-11-05'));
  const detail = screen.getByRole('dialog');
  h.resolve.mockRejectedValueOnce(new Error('boom'));
  await fireEvent.click(within(detail).getByRole('button', { name: 'Done EX-30' }));
  expect(await within(detail).findByRole('alert')).toHaveTextContent('Could not update this occurrence. Try again.');
  expect(screen.getAllByRole('alert')).toHaveLength(1);
 });
 it('names the day once in day detail', async () => {
  await mount(); await fireEvent.click(dateButton('2026-11-05'));
  expect(within(screen.getByRole('dialog')).getAllByRole('heading', { level: 2 })).toHaveLength(1);
 });
 it.each(['Snooze…', 'Move target…', 'Set forecast…'])('closes day detail before opening %s', async name => {
  await mount(); await fireEvent.click(dateButton('2026-11-05'));
  const detail = screen.getByRole('dialog');
  await fireEvent.click(within(detail).getByRole('button', { name: 'Actions EX-30' }));
  await fireEvent.click(screen.getByRole('menuitem', { name }));
  await waitFor(() => expect(screen.getAllByRole('dialog')).toHaveLength(1));
  expect(screen.getByRole('dialog')).not.toBe(detail);
 });
 it('keeps Skip pending and saved state in day detail until the quiet refresh lands', async () => {
  await mount(); await fireEvent.click(dateButton('2026-11-05'));
  const detail = screen.getByRole('dialog'); const refresh = deferred<AgendaResponse>();
  h.agenda.mockReturnValueOnce(refresh.promise);
  await fireEvent.click(within(detail).getByRole('button', { name: 'Actions EX-30' }));
  await fireEvent.click(screen.getByRole('menuitem', { name: 'Skip' }));
  await waitFor(() => expect(h.resolve).toHaveBeenCalledWith('work', 'EX-30', { occurrence_at: frozenAt, state: 'skip' }));
  await screen.findByText('Updating agenda…');
  expect(within(detail).getByRole('button', { name: 'Done EX-30' })).toBeDisabled();
  refresh.resolve(response([row({ state: 'skipped' })]));
  await said('Occurrence skipped.', 'success'); expect(screen.getByRole('dialog')).toBe(detail);
  expect(within(detail).getByText('Skipped')).toBeTruthy();
 });
 it('refocuses a keyboard month destination after data lands and cancels focus after a failed load', async () => {
  await mount(); const next = deferred<AgendaResponse>(); h.agenda.mockReturnValueOnce(next.promise);
  const last = dateButton('2026-11-30'); last.focus(); await fireEvent.keyDown(last, { key: 'PageDown' });
  await screen.findByText('Loading agenda…'); next.resolve(response([]));
  await waitFor(() => expect(document.activeElement).toBe(dateButton('2026-12-30')));
  h.agenda.mockRejectedValueOnce(new Error('offline'));
  await fireEvent.keyDown(document.activeElement!, { key: 'PageDown' }); await screen.findByText('Could not load this month');
  const outside = document.createElement('input'); document.body.appendChild(outside); outside.focus();
  h.agenda.mockResolvedValue(response([])); await fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
  await screen.findByRole('grid', { name: 'January 2027' }); await settle(); expect(document.activeElement).toBe(outside); outside.remove();
 });
 it('keeps the focused date when switching between desktop and phone', async () => {
  let change: ((event: MediaQueryListEvent) => void) | undefined;
  vi.spyOn(window, 'matchMedia').mockImplementation(query => ({ matches: false, media: query,
   addEventListener: (_name: string, callback: (event: MediaQueryListEvent) => void) => { if (query === '(max-width: 768px)') change = callback; },
   removeEventListener: vi.fn() }) as unknown as MediaQueryList);
  await mount(); dateButton('2026-11-05').focus();
  change!({ matches: true } as MediaQueryListEvent);
  await waitFor(() => expect(document.activeElement).toBe(document.querySelector('button.day-header[data-day="2026-11-05"]')));
  expect(screen.queryByRole('grid')).toBeNull();
  change!({ matches: false } as MediaQueryListEvent);
  await waitFor(() => expect(document.activeElement).toBe(dateButton('2026-11-05')));
  expect(h.agenda).toHaveBeenCalledTimes(1);
 });
 it('moves one month at a time and clamps March 31 to February 28 while keeping the selected day across modes', async () => {
  vi.mocked(Date.now).mockReturnValue(Date.parse('2026-03-31T12:00:00Z'));
  h.agenda.mockResolvedValue(response([row({ day: '2026-03-31' })]));
  await mount(); await fireEvent.click(screen.getByRole('button', { name: 'Previous month' }));
  await waitFor(() => expect(h.agenda).toHaveBeenLastCalledWith('2026-02-01T05:00:00.000Z', '2026-03-01T05:00:00.000Z', 'America/New_York', 'all', ['completed']));
  expect(dateButton('2026-02-28')).toHaveAttribute('tabindex', '0');
  await fireEvent.click(screen.getByRole('button', { name: 'Day' }));
  await waitFor(() => expect(h.agenda).toHaveBeenLastCalledWith('2026-02-28T05:00:00.000Z', '2026-03-01T05:00:00.000Z', 'America/New_York', 'all', ['completed']));
  await fireEvent.click(screen.getByRole('button', { name: 'Month' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Next month' }));
  await waitFor(() => expect(h.agenda).toHaveBeenLastCalledWith('2026-03-01T05:00:00.000Z', '2026-04-01T04:00:00.000Z', 'America/New_York', 'all', ['completed']));
  expect(dateButton('2026-03-28')).toHaveAttribute('tabindex', '0');
  await fireEvent.click(screen.getByRole('button', { name: 'Today' }));
  expect(dateButton('2026-03-31')).toHaveAttribute('tabindex', '0');
 });
 it('marks an incomplete tail and hides previews after next_from without claiming an empty month', async () => {
  h.agenda.mockResolvedValue(response([row({ day: '2026-11-16', item_title: 'Unexamined tail' })], { truncated: true, next_from: '2026-11-15T17:00:00Z' }));
  render(Agenda); await screen.findByText('Showing entries up to Sun 15 November.');
  expect(within(cellOf('2026-11-14')).queryByText('Not fully loaded')).toBeNull();
  for (const day of ['2026-11-15', '2026-11-16', '2026-11-30']) {
   expect(within(cellOf(day)).getByText('Not fully loaded')).toBeTruthy();
   expect(dateButton(day).getAttribute('aria-label')).toMatch(/, Incomplete$/);
  }
  expect(screen.queryByText('Unexamined tail')).toBeNull(); expect(screen.queryByText('No entries this month.')).toBeNull();
  await fireEvent.click(dateButton('2026-11-16')); expect(within(screen.getByRole('dialog')).getByText('Not fully loaded')).toBeTruthy();
  expect(within(screen.getByRole('dialog')).queryByText('No entries on this day.')).toBeNull();
 });
 it('shows the complete empty month and complete empty day detail', async () => {
  h.agenda.mockResolvedValue(response([])); render(Agenda); await screen.findByText('No entries this month.');
  await fireEvent.click(dateButton('2026-11-05'));
  expect(within(screen.getByRole('dialog')).getByText('No entries on this day.')).toBeTruthy();
 });
 it('offers to reveal hidden completed entries in Month', async () => {
  h.agenda.mockResolvedValue(response([row({ type: 'completed' }), row({ type: 'completed', item_ref: 'EX-31' })]));
  render(Agenda); await screen.findByRole('grid'); await toggleCompleted();
  expect(screen.getByText('Completed work is hidden.')).toBeTruthy();
  expect(screen.queryByText('No entries this month.')).toBeNull();
  // Shown again, they are still not lines in the cell: the day detail folds them into its last line.
  await fireEvent.click(screen.getByRole('button', { name: 'Show it' })); expect(screen.queryByText('First item')).toBeNull();
  await fireEvent.click(dateButton('2026-11-05'));
  await fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: '2 completed' }));
  expect(within(screen.getByRole('dialog')).getAllByText('First item')).toHaveLength(2);
 });
 it('shows the summary below the grid, named for the month, and no outstanding list', async () => {
  h.agenda.mockResolvedValue(response([row()], { priority_list: [workRow()], counts: [{ day: '2026-11-05', completed: 1, open: 2, burndown: 2 }] })); await mount();
  const summary = screen.getByRole('region', { name: 'This month' });
  expect(screen.getByRole('grid').compareDocumentPosition(summary) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  expect(summary.closest('aside')).toBeNull(); expect(summary.querySelector('[data-stat="outstanding"] b')?.textContent).toBe('1');
  expect(screen.queryByRole('region', { name: 'Outstanding' })).toBeNull();
 });
 // Completed entries of one day, as the agenda returns them with the option on.
 const made = (count: number, day = '2026-11-05') => Array.from({ length: count }, (_, i) => row({ type: 'completed', item_ref: `EV-${i}`, item_title: `Made ${i}`, schedule_id: undefined, state: undefined, day, at: `${day}T15:00:00Z` }));
 const three = () => [0, 1, 2].map(i => row({ item_ref: `EX-${30 + i}`, schedule_id: `s${i}`, item_title: `Plain ${i}` }));
 it('counts and lists commitments only in a cell, and folds completed entries into a disclosure in day detail', async () => {
  h.agenda.mockResolvedValue(response([...three(), ...made(20)])); render(Agenda); await screen.findByRole('grid');
  const cell = cellOf('2026-11-05');
  expect(cell.querySelector('.count')).toHaveTextContent(/^3 · 3 owed$/);
  expect(within(cell).getAllByRole('link').map(line => line.querySelector('.t')?.textContent)).toEqual(['Plain 0', 'Plain 1', 'Plain 2']);
  expect(within(cell).queryByRole('button', { name: /more$/ })).toBeNull();
  expect(dateButton('2026-11-05').getAttribute('aria-label')).toMatch(/, Today, 3 entries$/);
  await fireEvent.click(dateButton('2026-11-05'));
  const detail = screen.getByRole('dialog', { name: 'Thursday, 5 November 2026, Today' });
  expect(detail.querySelector('.subject')).toHaveTextContent(/^3 entries · 3 owed$/);
  expect([...detail.querySelectorAll('[data-ref]')].map(el => el.getAttribute('data-ref'))).toEqual(['EX-30', 'EX-31', 'EX-32']);
  const toggle = within(detail).getByRole('button', { name: '20 completed' });
  expect(toggle).toHaveAttribute('aria-expanded', 'false');
  await fireEvent.click(toggle);
  expect(toggle).toHaveAttribute('aria-expanded', 'true');
  expect(detail.querySelectorAll('[data-ref^="EV-"]')).toHaveLength(20);
  expect(detail.querySelector('.subject')).toHaveTextContent(/^3 entries · 3 owed$/);
 });
 it('leaves a day with only completed entries empty in the grid, with them in its day detail', async () => {
  h.agenda.mockResolvedValue(response([row(), ...made(2, '2026-11-09')])); render(Agenda); await screen.findByRole('grid');
  const cell = cellOf('2026-11-09');
  expect(cell.querySelector('.count')).toBeNull(); expect(within(cell).queryAllByRole('link')).toHaveLength(0);
  expect(dateButton('2026-11-09').getAttribute('aria-label')).toMatch(/2026, 0 entries$/);
  await fireEvent.click(dateButton('2026-11-09'));
  const detail = screen.getByRole('dialog', { name: 'Monday, 9 November 2026' });
  expect(detail.querySelector('.subject')).toBeNull(); expect(within(detail).queryByText('No entries on this day.')).toBeNull();
  expect(within(detail).getByRole('button', { name: '2 completed' })).toHaveAttribute('aria-expanded', 'false');
 });
 it('on a phone keeps the header of a day with only completed entries while they are shown, and folds it once they are hidden', async () => {
  phone(); h.agenda.mockResolvedValue(response([...three(), ...made(20), ...made(2, '2026-11-09')])); render(Agenda);
  const list = await screen.findByRole('list', { name: 'November 2026' });
  const headers = () => [...list.querySelectorAll('button.day-header')].map(button => button.getAttribute('data-day'));
  expect(headers()).toEqual(['2026-11-05', '2026-11-09']);
  const quiet = list.querySelector('button.day-header[data-day="2026-11-09"]')!.closest('li')!;
  expect(quiet.querySelectorAll('[data-ref]')).toHaveLength(0);
  const toggle = within(quiet).getByRole('button', { name: '2 completed' });
  expect(toggle).toHaveAttribute('aria-expanded', 'false');
  await fireEvent.click(toggle); expect(quiet.querySelectorAll('[data-ref^="EV-"]')).toHaveLength(2);
  const today = list.querySelector('button.day-header[data-day="2026-11-05"]')!.closest('li')!;
  expect([...today.querySelectorAll('[data-ref]')].map(el => el.getAttribute('data-ref'))).toEqual(['EX-30', 'EX-31', 'EX-32']);
  expect(within(today).getByRole('button', { name: '20 completed' })).toHaveAttribute('aria-expanded', 'false');
  await toggleCompleted();
  await waitFor(() => expect(headers()).toEqual(['2026-11-05']));
  expect(within(list).queryByRole('button', { name: /completed/ })).toBeNull();
 });
 it('heads a phone day as the week does: the day without its year, a Today badge and what is owed', async () => {
  phone(); h.agenda.mockResolvedValue(response([row(), row({ item_ref: 'EX-31', schedule_id: 's2', state: 'done' }), row({ item_ref: 'EX-50', schedule_id: 's3', day: '2026-11-09', at: '2026-11-09T14:00:00Z' })])); render(Agenda);
  const list = await screen.findByRole('list', { name: 'November 2026' });
  const today = list.querySelector('button.day-header[data-day="2026-11-05"]')!;
  expect(today.textContent?.replace(/\s+/g, ' ').trim()).toBe('Thursday 5 November Today 1 owed · 1 done');
  expect(today.querySelector('.badge')).toHaveTextContent('Today');
  expect(list.querySelector('button.day-header[data-day="2026-11-09"]')!.textContent?.replace(/\s+/g, ' ').trim()).toBe('Monday 9 November 1 owed');
  expect(list.textContent).not.toMatch(/\d entr/);
 });
 it('says "1 entry" for a day with one entry, in the cell label and the day detail', async () => {
  await mount();
  expect(dateButton('2026-11-05').getAttribute('aria-label')).toMatch(/, Today, 1 entry$/);
  await fireEvent.click(dateButton('2026-11-05'));
  expect(screen.getByRole('dialog').querySelector('.subject')).toHaveTextContent(/^1 entry · 1 owed$/);
 });
 it('uses the phone day list and its summaries instead of the desktop grid', async () => {
  phone();
  h.agenda.mockResolvedValue(response([row()], { unavailable: [{ item_ref: 'EX-99', code: 'internal' }] }));
  await mount(); expect(screen.queryByRole('grid')).toBeNull();
  const list = screen.getByRole('list', { name: 'November 2026' }); expect(list.children).toHaveLength(30);
  const header = within(list).getByRole('button', { name: /^Thursday 5 November\s*Today.*Not fully loaded/ });
  await fireEvent.click(header); expect(within(screen.getByRole('dialog')).getByRole('button', { name: 'Done EX-30' })).toBeTruthy();
 });
});

describe('created entries stay off the agenda; Show completed is its own toggle (TASK-4512)', () => {
 const phone = () => vi.spyOn(window, 'matchMedia').mockImplementation(query => ({ matches: query === '(max-width: 768px)', media: query, addEventListener: vi.fn(), removeEventListener: vi.fn() }) as unknown as MediaQueryList);
 const event = (type: 'created' | 'completed', ref: string, title: string, day = '2026-11-05') => row({ type, item_ref: ref, item_title: title, schedule_id: undefined, state: undefined, roles: undefined, day, at: `${day}T15:00:00Z` });
 // One commitment, two completed and two created on its day, and a day whose only entry is a created one.
 const mixed = () => response([row(), event('completed', 'EV-1', 'Closed one'), event('completed', 'EV-2', 'Closed two'), event('created', 'EV-3', 'Made one'), event('created', 'EV-4', 'Made two'), event('created', 'EV-5', 'Made alone', '2026-11-07')]);
 const noCreated = () => { for (const title of ['Made one', 'Made two', 'Made alone']) expect(screen.queryByText(title)).toBeNull(); expect(document.body.textContent).not.toMatch(/created/i); };

 it.each(['week', 'day'])('%s: never draws a created entry, in the rows, the disclosure, the strip or its counts', async mode => {
  localStorage.setItem('pad-agenda-mode:user-a', mode); h.agenda.mockResolvedValue(mixed()); await mount();
  const toggle = screen.getByRole('button', { name: '2 completed' });
  expect(toggle).toHaveAttribute('aria-expanded', 'false'); // The chevron follows the words: "2 completed ›".
  expect(toggle.textContent?.trim()).toBe('2 completed'); expect(toggle.lastElementChild?.tagName.toLowerCase()).toBe('svg');
  await fireEvent.click(toggle); expect(toggle).toHaveAttribute('aria-expanded', 'true');
  expect(screen.getByText('Closed one')).toBeTruthy(); expect(screen.getByText('Closed two')).toBeTruthy(); noCreated();
  expect(stripDay('2026-11-05')).toHaveAccessibleName(/Thursday 5 November, Today, 1 entry, 1 owed/);
  // Day range loads one day; in Week the day whose only entry is a created one reads as empty.
  if (mode === 'week') {
   expect(stripDay('2026-11-07')).toHaveAccessibleName(/Saturday 7 November, no entries/); expect(stripDay('2026-11-07').querySelectorAll('.pip')).toHaveLength(0);
   expect(within(daySection('2026-11-07')).getByText('Nothing scheduled.')).toBeTruthy();
  }
 });
 it('month: never draws a created entry in a cell, the day dialog or the empty days', async () => {
  localStorage.setItem('pad-agenda-mode:user-a', 'month'); h.agenda.mockResolvedValue(mixed()); render(Agenda); await screen.findByRole('grid');
  noCreated();
  await fireEvent.click(document.querySelector('button.date[data-day="2026-11-05"]') as HTMLButtonElement);
  const detail = screen.getByRole('dialog');
  const toggle = within(detail).getByRole('button', { name: '2 completed' }); await fireEvent.click(toggle);
  expect([...detail.querySelectorAll('[data-ref]')].map(el => el.getAttribute('data-ref'))).toEqual(['EX-30', 'EV-1', 'EV-2']); noCreated();
  await fireEvent.click(within(detail).getByRole('button', { name: 'Close' }));
  await fireEvent.click(document.querySelector('button.date[data-day="2026-11-07"]') as HTMLButtonElement);
  expect(within(screen.getByRole('dialog')).getByText('No entries on this day.')).toBeTruthy();
 });
 it('phone month list: no created entry, and a day with only created ones folds as empty', async () => {
  localStorage.setItem('pad-agenda-mode:user-a', 'month'); phone(); h.agenda.mockResolvedValue(mixed()); render(Agenda);
  const list = await screen.findByRole('list', { name: 'November 2026' });
  expect([...list.querySelectorAll('button.day-header')].map(button => button.getAttribute('data-day'))).toEqual(['2026-11-05']);
  await fireEvent.click(within(list).getByRole('button', { name: '2 completed' }));
  expect([...list.querySelectorAll('[data-ref]')].map(el => el.getAttribute('data-ref'))).toEqual(['EX-30', 'EV-1', 'EV-2']); noCreated();
 });
 it('Show completed defaults on, toggles off and persists under its own key, and ignores the old combined key', async () => {
  localStorage.setItem('pad-agenda-show-events:user-a', '0');
  h.agenda.mockResolvedValue(mixed()); await mount();
  expect((await completedOption()).getAttribute('aria-checked')).toBe('true');
  expect(screen.queryByRole('menuitemradio', { name: 'Show created and completed' })).toBeNull();
  await fireEvent.click(screen.getByRole('button', { name: 'View options' }));
  expect(screen.getByRole('button', { name: '2 completed' })).toBeTruthy();
  await toggleCompleted();
  expect(screen.queryByRole('button', { name: /completed/ })).toBeNull(); expect(screen.getByText('First item')).toBeTruthy();
  expect(localStorage.getItem('pad-agenda-completed:user-a')).toBe('0'); expect(localStorage.getItem('pad-agenda-show-events:user-a')).toBe('0');
  cleanup(); await mount(); expect(screen.queryByRole('button', { name: /completed/ })).toBeNull();
  await toggleCompleted(); expect(localStorage.getItem('pad-agenda-completed:user-a')).toBe('1');
  expect(screen.getByRole('button', { name: '2 completed' })).toBeTruthy();
 });
 it.each([true, false])('a dated item done on its day stays inline as Done, Show completed %s', async shown => {
  if (!shown) localStorage.setItem('pad-agenda-completed:user-a', '0');
  h.agenda.mockResolvedValue(response([row({ state: 'done', item_title: 'Finished occurrence' }), event('completed', 'EX-30', 'Finished occurrence')]));
  render(Agenda); const link = await screen.findByRole('link', { name: 'Finished occurrence' });
  const inline = link.closest('li.agenda-row')!;
  expect(inline.classList.contains('done')).toBe(true); expect(within(inline as HTMLElement).getByRole('img', { name: 'Done' })).toBeTruthy();
  expect(inline.closest('li:not(.agenda-row)')).toBeNull();
  expect(screen.queryAllByRole('button', { name: '1 completed' })).toHaveLength(shown ? 1 : 0);
 });
 it('a window of created entries only reads as empty, with no hidden notice', async () => {
  h.agenda.mockResolvedValue(response([event('created', 'EV-3', 'Made one')])); render(Agenda);
  expect(await screen.findByText('Nothing scheduled for 5–11 November 2026')).toBeTruthy();
  expect(screen.queryByText(/hidden/)).toBeNull(); expect(screen.queryByRole('button', { name: /^Show/ })).toBeNull(); noCreated();
 });
 it('a window of completed entries only, with Show completed off, says so and offers to show it', async () => {
  localStorage.setItem('pad-agenda-completed:user-a', '0');
  h.agenda.mockResolvedValue(response([event('completed', 'EV-1', 'Closed one'), event('created', 'EV-3', 'Made one')])); render(Agenda);
  expect(await screen.findByText('Completed work is hidden.')).toBeTruthy(); expect(screen.queryByText(/Nothing scheduled/)).toBeNull();
  await fireEvent.click(screen.getByRole('button', { name: 'Show it' }));
  expect(screen.queryByText('Completed work is hidden.')).toBeNull();
  await fireEvent.click(await screen.findByRole('button', { name: '1 completed' })); expect(screen.getByText('Closed one')).toBeTruthy(); noCreated();
 });
});

describe('the last occurrence of an open item (TASK-4537)', () => {
 beforeEach(() => localStorage.setItem('pad-agenda-mode:user-a', 'day'));
 const stillOpen = (extra: Record<string, unknown> = {}) => ({ code: 'item_still_open', message: 'This is the last occurrence.', details: { state: 'done', occurrence_at: frozenAt, done_field: 'status', item_status: 'open', close_options: ['done'], ack_available: false, ...extra } });
 const ask = async (extra: Record<string, unknown> = {}) => {
  h.resolve.mockRejectedValueOnce(stillOpen(extra)); h.update.mockResolvedValue({});
  await mount(); await fireEvent.click(screen.getByRole('button', { name: 'Done EX-30' }));
  return screen.findByRole('dialog', { name: 'Last occurrence: the item is still open' });
 };
 it('Close item too ends the schedule first, then closes the item', async () => {
  const d = await ask();
  await fireEvent.click(within(d).getByRole('button', { name: 'Close item' }));
  await waitFor(() => expect(h.update).toHaveBeenCalledWith('work', 'EX-30', { fields_patch: { status: 'done' } }));
  expect(h.resolve).toHaveBeenNthCalledWith(2, 'work', 'EX-30', { occurrence_at: frozenAt, state: 'done', end_schedule: true });
  expect(h.resolve.mock.invocationCallOrder[1]).toBeLessThan(h.update.mock.invocationCallOrder[0]);
  await said('Occurrence marked done; the schedule has ended and the item is closed.', 'success');
  await waitFor(() => expect(screen.queryByRole('dialog', { name: /Last occurrence/ })).toBeNull());
 });
 it('End schedule keeps the item open', async () => {
  const d = await ask();
  await fireEvent.click(within(d).getByLabelText(/End schedule, keep item open/));
  await fireEvent.click(within(d).getByRole('button', { name: 'End schedule' }));
  await waitFor(() => expect(h.resolve).toHaveBeenCalledTimes(2));
  expect(h.resolve).toHaveBeenLastCalledWith('work', 'EX-30', { occurrence_at: frozenAt, state: 'done', end_schedule: true });
  expect(h.update).not.toHaveBeenCalled();
  await said('Occurrence marked done; the schedule has ended. The item stays open in Outstanding.', 'success');
 });
 it('Acknowledge instead acknowledges and writes no ending', async () => {
  const d = await ask({ ack_available: true });
  await fireEvent.click(within(d).getByLabelText(/Acknowledge instead/));
  await fireEvent.click(within(d).getByRole('button', { name: 'Acknowledge' }));
  await waitFor(() => expect(h.resolve).toHaveBeenCalledTimes(2));
  expect(h.resolve).toHaveBeenLastCalledWith('work', 'EX-30', { occurrence_at: frozenAt, state: 'ack' });
  expect(h.update).not.toHaveBeenCalled();
 });
 it('Cancel writes nothing', async () => {
  const d = await ask();
  await fireEvent.click(within(d).getByRole('button', { name: 'Cancel' }));
  await waitFor(() => expect(screen.queryByRole('dialog', { name: /Last occurrence/ })).toBeNull());
  await settle();
  expect(h.resolve).toHaveBeenCalledTimes(1); expect(h.update).not.toHaveBeenCalled();
 });
 it('a close refused after the schedule ended says so and refreshes', async () => {
  const d = await ask(); h.update.mockRejectedValue({ code: 'open_children', message: 'EX-30 has open children' });
  await fireEvent.click(within(d).getByRole('button', { name: 'Close item' }));
  await said('The schedule has ended, but closing the item failed: EX-30 has open children. It stays open in Outstanding.');
  expect(h.agenda.mock.calls.length).toBeGreaterThanOrEqual(2);
 });
 it('Dismiss on a schedule_ended Outstanding row', async () => {
  localStorage.setItem('pad-agenda-mode:user-a', 'week');
  const ended = workRow({ type: 'item', reason: 'schedule_ended', ended_at: '2026-11-04T15:00:00Z', roles: undefined, at: undefined, day: undefined, state: undefined });
  h.agenda.mockResolvedValueOnce(response([row()], { priority_list: [ended] })).mockResolvedValue(response([row()]));
  h.dismiss.mockResolvedValue({ commitment: {} });
  await mount();
  const item = document.querySelector<HTMLElement>('li[data-work="EX-30"]')!;
  expect(item).toHaveTextContent('Schedule ended');
  await fireEvent.click(within(item).getByRole('button', { name: 'Actions for EX-30' }));
  await fireEvent.click(screen.getByRole('menuitem', { name: 'Dismiss' }));
  await waitFor(() => expect(h.dismiss).toHaveBeenCalledWith('work', 'EX-30'));
  await said('Dismissed. EX-30 stays open.', 'success');
 });
});
