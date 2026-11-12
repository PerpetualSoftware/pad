import { beforeEach, afterEach, describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent, waitFor, cleanup, within } from '@testing-library/svelte';
import { flushSync } from 'svelte';
import type { AgendaEntry, AgendaResponse, AgendaWorkEntry, CommitmentMoveResult } from '$lib/types';

const h = vi.hoisted(() => ({ agenda: vi.fn(), view: vi.fn(), resolve: vi.fn(), setForecast: vi.fn(), clearForecast: vi.fn(), preview: vi.fn(), move: vi.fn(), item: vi.fn(), user: { id: 'user-a', timezone: 'America/New_York' } as { id: string; timezone?: string } | null, epoch: 0, signal: null as null | { set(key: string, value: number): unknown }, browserZone: 'Europe/London' as string | null }));
vi.mock('$lib/api/client', () => ({ api: { getMyAgenda: h.agenda, getCommitment: h.view, setCommitmentForecast: h.setForecast, clearCommitmentForecast: h.clearForecast, resolveCommitment: h.resolve, previewCommitmentMove: h.preview, moveCommitment: h.move, items: { get: h.item } } }));
vi.mock('$lib/timezone', () => ({ browserZone: () => h.browserZone }));
vi.mock('$lib/stores/auth.svelte', async () => {
 const { SvelteMap } = await import('svelte/reactivity');
 const signal = new SvelteMap<string, number>(); h.signal = signal;
 return { authStore: {
  get user() { signal.get('auth'); return h.user; },
  get identityEpoch() { signal.get('auth'); return h.epoch; },
  identityFence() { const epoch = h.epoch; return () => epoch === h.epoch; }
 } };
});
import Agenda from './+page.svelte';

const row = (extra: Partial<AgendaEntry> = {}): AgendaEntry => ({ type: 'schedule', at: '2026-11-05T14:00:00Z', day: '2026-11-05', wall: '2026-11-05T09:00:00', floating: true, item_ref: 'EX-30', item_title: 'First item', collection: 'tasks', workspace: 'work', workspace_name: 'First workspace', owner_username: 'alice', state: 'outstanding', roles: { target: null, earliest: null, deadline: null, deadline_behavior: null, occupied_until: null, outcome: '', roles_origin: 'native' }, schedule_id: 'schedule-1', ...extra });
const frozenAt = '2026-11-05T17:00:00Z';
const response = (entries: AgendaEntry[] = [row()], extra: Partial<AgendaResponse> = {}): AgendaResponse => ({ from: '', to: '', tz: 'America/New_York', entries, window_complete:true,priority_list:[],priority_list_complete:true,waiting_on:[],waiting_on_complete:true,no_date:[],no_date_complete:true, ...extra });
const deferred = <T,>() => { let resolve!: (value: T) => void; const promise = new Promise<T>(r => { resolve = r; }); return { promise, resolve }; };
const settle = async () => { await new Promise(r => setTimeout(r, 0)); flushSync(); };
async function mount() { render(Agenda); await screen.findByText('First item'); }
async function menu(ref = 'EX-30') { await fireEvent.click(screen.getByRole('button', { name: `Actions ${ref}` })); }
async function dialog(kind: 'Move' | 'Snooze') {
 await menu();
 await fireEvent.click(screen.getByRole('menuitem', { name: kind === 'Move' ? 'Move target…' : 'Snooze…' }));
 return screen.getByRole('dialog', { name: kind === 'Move' ? 'Move target' : 'Snooze occurrence' });
}
async function fillTime(d: HTMLElement, value = '2026-11-06T09:30') { await fireEvent.input(within(d).getByLabelText(/New date and time|Remind me again at/), { target: { value } }); }

const preview = (to = '2026-11-06T14:00:00.000Z', extra: Partial<CommitmentMoveResult> = {}): CommitmentMoveResult => ({ revision: 'revision-1', changes: [{ item_id: 'root', role: 'target', from: row().at, to, root: true }], ...extra });
const workRow = (extra: Partial<AgendaWorkEntry> = {}): AgendaWorkEntry => ({ ...row(), type: 'schedule', rank: 'normal', reason: 'outstanding', ...extra });
const card = (ref = 'EX-30') => document.querySelector(`[data-ref="${ref}"][draggable="true"]`) as HTMLElement;
const daySection = (day = '2026-11-06') => document.querySelector(`[data-day="${day}"]`) as HTMLElement;
async function drag(day = '2026-11-06', ref = 'EX-30') {
 await fireEvent.dragStart(card(ref)); await fireEvent.dragEnter(daySection(day));
}
async function drop(day = '2026-11-06', ref = 'EX-30') { await drag(day, ref); await fireEvent.drop(daySection(day)); }

beforeEach(() => {
 vi.resetAllMocks(); localStorage.clear(); h.user = { id: 'user-a', timezone: 'America/New_York' }; h.epoch = 0; h.browserZone = 'Europe/London';
 vi.spyOn(Date, 'now').mockReturnValue(Date.parse('2026-11-05T12:00:00Z'));
 h.agenda.mockResolvedValue(response());
 h.view.mockResolvedValue({ item_ref: 'EX-30', roles: { target: null, earliest: null, deadline: null, deadline_behavior: null, occupied_until: null, outcome: '', roles_origin: 'native' }, floating: true, next_occurrence: { at: frozenAt, wall: row().wall } });
 h.preview.mockImplementation((_ws, _ref, body) => Promise.resolve(preview(body.to)));
 h.move.mockResolvedValue(preview()); h.item.mockResolvedValue({ title: 'Linked child' });
 h.resolve.mockResolvedValue({ schedule: {}, resolution: { occurrence_at: frozenAt, state: 'done', missed_recorded: 0 } });
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); });

describe('agenda Day page', () => {
 beforeEach(() => localStorage.setItem('pad-agenda-mode:user-a', 'day'));
 it('renders two workspaces, routes, time, checkbox and one polite status', async () => {
  h.agenda.mockResolvedValue(response([row(), row({ workspace: 'other', owner_username: 'bob', workspace_name: 'Second workspace', item_ref: 'EX-31', item_title: 'Second item', schedule_id: 'schedule-2', all_day: true })]));
  await mount();
  expect(screen.getByText('First workspace / tasks')).toBeTruthy(); expect(screen.getByText('Second workspace / tasks')).toBeTruthy();
  expect(screen.getByRole('link', { name: 'First item' }).getAttribute('href')).toBe('/alice/work/tasks/EX-30');
  expect(screen.getByText('09:00')).toBeTruthy(); expect(screen.getByText('All day')).toBeTruthy();
  expect((screen.getByRole('checkbox', { name: 'Show created and completed' }) as HTMLInputElement).checked).toBe(true);
  expect(screen.getAllByRole('status')).toHaveLength(1);
  expect(screen.getByRole('link', { name: 'America/New_York' }).getAttribute('href')).toBe('/console/settings');
  expect(h.agenda.mock.calls[0][2]).toBe('America/New_York');
  expect(screen.queryByRole('button', { name: 'Month' })).toBeNull();
 });
 it('hides only events locally, persists per user, and another user defaults to checked', async () => {
  h.agenda.mockResolvedValue(response([row(), row({ type: 'created', item_title: 'Created item' }), row({ type: 'completed', item_title: 'Completed item' })]));
  await mount(); await fireEvent.click(screen.getByRole('checkbox'));
  expect(screen.queryByText('Created item')).toBeNull(); expect(screen.queryByText('Completed item')).toBeNull(); expect(screen.getByText('First item')).toBeTruthy();
  expect(localStorage.getItem('pad-agenda-show-events:user-a')).toBe('0'); expect(h.agenda).toHaveBeenCalledTimes(1);
  cleanup(); await mount(); expect(screen.queryByText('Created item')).toBeNull();
  cleanup(); h.user = { id: 'user-b', timezone: 'America/New_York' }; await mount(); expect(screen.getByText('Created item')).toBeTruthy();
 });
 it('refetches timezone changes on the mounted page and isolates a new user preference', async () => {
  await mount(); await fireEvent.click(screen.getByRole('checkbox'));
  h.user = { id: 'user-a', timezone: 'Asia/Tokyo' }; h.signal?.set('auth', 1);
  await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(2));
  expect(h.agenda.mock.calls[1]).toEqual(['2026-11-04T15:00:00.000Z', '2026-11-05T15:00:00.000Z', 'Asia/Tokyo']);
  h.user = { id: 'user-b', timezone: 'UTC' }; h.epoch++; h.signal?.set('auth', 2);
  await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(3));
  expect((screen.getByRole('checkbox') as HTMLInputElement).checked).toBe(true);
  expect(localStorage.getItem('pad-agenda-show-events:user-b')).toBeNull();
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
  expect(done.parentElement?.classList.contains('done')).toBe(true); expect(screen.getByLabelText('Done')).toHaveTextContent('✓');
  expect(screen.getByText('10:30')).toBeTruthy(); expect(screen.getByText('No owner')).toBeTruthy();
  expect(screen.queryByRole('link', { name: 'No owner' })).toBeNull();
 });
 it('survives throwing storage with an in-memory default', async () => {
  vi.spyOn(localStorage, 'getItem').mockImplementation(() => { throw new Error('blocked'); });
  vi.spyOn(localStorage, 'setItem').mockImplementation(() => { throw new Error('blocked'); });
  await mount(); expect((screen.getByRole('checkbox') as HTMLInputElement).checked).toBe(true);
  await fireEvent.click(screen.getByRole('checkbox')); expect((screen.getByRole('checkbox') as HTMLInputElement).checked).toBe(false);
 });
 it('does not access the preference before the user is known', async () => {
  h.user = null; const get = vi.spyOn(localStorage, 'getItem'); const set = vi.spyOn(localStorage, 'setItem');
  render(Agenda); await settle(); expect(get).not.toHaveBeenCalled(); expect(set).not.toHaveBeenCalled(); expect(h.agenda).not.toHaveBeenCalled();
 });
 it.each([{ truncated: true, next_from: '2026-11-06T01:00:00Z' }, { unavailable: [{ item_ref: 'EX-30', code: 'internal' }] }, { unexpanded: [{ schedule_id: 's', item_ref: 'EX-30', item_title: 'X', collection: 'tasks', reason: 'scan_limit' as const }] }])('never claims an incomplete answer is empty: %j', async extra => {
  h.agenda.mockResolvedValue(response([], extra)); render(Agenda);
  await screen.findByText('Agenda is incomplete.'); expect(screen.queryByText('No entries on this day.')).toBeNull();
  if ('next_from' in extra) expect(screen.getByText('more after November 5, 2026')).toBeTruthy();
 });
 it('offers to show hidden events when they are everything loaded', async () => {
  h.agenda.mockResolvedValue(response([row({ type: 'created' })])); await mount(); await fireEvent.click(screen.getByRole('checkbox'));
  expect(screen.getByText('Created and completed entries are hidden.')).toBeTruthy();
  await fireEvent.click(screen.getByRole('button', { name: 'Show created and completed' })); expect(screen.getByText('First item')).toBeTruthy();
 });
 it('renders loading, error with retry, and the complete empty state', async () => {
  const pending = deferred<AgendaResponse>(); h.agenda.mockReturnValueOnce(pending.promise); render(Agenda); await screen.findByText('Loading agenda…');
  pending.resolve(response([])); await screen.findByText('No entries on this day.');
  cleanup(); h.agenda.mockRejectedValueOnce(new Error('offline')); render(Agenda); await screen.findByText('Could not load agenda.');
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
  expect(h.resolve).toHaveBeenCalledTimes(1); expect(h.view).toHaveBeenCalledWith('work', 'EX-30'); await screen.findByText('Occurrence marked done.');
 });
 it('Skip submits once with the view identity', async () => {
  await mount(); await menu(); expect(h.view).not.toHaveBeenCalled(); await fireEvent.click(screen.getByRole('menuitem', { name: 'Skip' }));
  await waitFor(() => expect(h.resolve).toHaveBeenCalledWith('work', 'EX-30', { occurrence_at: frozenAt, state: 'skip' })); await screen.findByText('Occurrence skipped.');
 });
 it.each(['different wall', 'ended', 'missing occurrence', '404'])('refuses a %s view as stale', async kind => {
  if (kind === '404') h.view.mockRejectedValue({ code: 'not_found' });
  else h.view.mockResolvedValue({ ended_at: kind === 'ended' ? '2026-11-05T12:00:00Z' : undefined, next_occurrence: kind === 'missing occurrence' ? undefined : { at: frozenAt, wall: kind === 'different wall' ? '2026-11-06T09:00:00' : row().wall } });
  await mount(); await fireEvent.click(screen.getByRole('button', { name: 'Done EX-30' })); await screen.findByText('This occurrence changed. Review the updated entry and try again.');
  expect(h.resolve).not.toHaveBeenCalled(); await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(2));
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
  write.resolve({}); await screen.findByText('Occurrence marked done.');
 });
 it.each([['resolve', 'stale', 'This occurrence changed. Review the updated entry and try again.'], ['resolve', 'forbidden', 'You can no longer change this item.'], ['view', 'forbidden', 'You can no longer change this item.'], ['view', 'internal', 'Could not update this occurrence. Try again.']])('handles %s %s without replay', async (source, code, message) => {
  h[source === 'view' ? 'view' : 'resolve'].mockRejectedValue({ code }); await mount(); await fireEvent.click(screen.getByRole('button', { name: 'Done EX-30' })); await screen.findByText(message);
  if (code !== 'internal') await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(2));
  expect(h.resolve).toHaveBeenCalledTimes(source === 'view' ? 0 : 1);
 });
 it('shows the server sentence when a skip before a hard deadline needs the owner, and keeps the button', async () => {
  const sentence = 'Only the assignee or a workspace owner can skip this occurrence before its hard deadline.';
  h.resolve.mockRejectedValue({ code: 'deadline_owner_required', message: sentence }); await mount(); await fireEvent.click(screen.getByRole('button', { name: 'Done EX-30' })); await screen.findByText(sentence);
  expect(h.agenda).toHaveBeenCalledTimes(1); expect((screen.getByRole('button', { name: 'Done EX-30' }) as HTMLButtonElement).disabled).toBe(false);
 });
 it('keeps rows on quiet refresh and retries only the read after a successful write', async () => {
  const refresh = deferred<AgendaResponse>(); h.agenda.mockResolvedValueOnce(response()).mockReturnValueOnce(refresh.promise); await mount();
  await fireEvent.click(screen.getByRole('button', { name: 'Done EX-30' })); await screen.findByText('Updating agenda…'); expect(screen.getByText('First item')).toBeTruthy();
  refresh.resolve(response()); await screen.findByText('Occurrence marked done.');
  h.agenda.mockRejectedValueOnce(new Error('offline')); await fireEvent.click(screen.getByRole('button', { name: 'Done EX-30' })); await screen.findByText('Saved. Could not refresh the view.');
  const writes = h.resolve.mock.calls.length; h.agenda.mockResolvedValue(response()); await fireEvent.click(screen.getByRole('button', { name: 'Try again' })); await settle(); expect(h.resolve).toHaveBeenCalledTimes(writes);
 });
 it('does not resolve after identity changes during the schedule read', async () => {
  const view = deferred<unknown>(); h.view.mockReturnValueOnce(view.promise); await mount(); await fireEvent.click(screen.getByRole('button', { name: 'Done EX-30' }));
  h.epoch++; view.resolve({ next_occurrence: { at: frozenAt, wall: row().wall } }); await settle(); expect(h.resolve).not.toHaveBeenCalled();
 });
});

describe('agenda Week and priority', () => {
 it('an incomplete week never says a day is empty', async () => {
  h.agenda.mockResolvedValue(response([], { window_complete: false, truncated: true }));
  render(Agenda);
  await screen.findByText('Agenda is incomplete.');
  expect(document.querySelectorAll('[data-day]')).toHaveLength(7);
  expect(screen.queryByText('Nothing on this day.')).toBeNull();
 });
 it('defaults to one seven-local-day DST window, renders seven days and shifts by civil weeks', async () => {
  vi.mocked(Date.now).mockReturnValue(Date.parse('2026-10-30T16:00:00Z'));
  h.agenda.mockResolvedValue(response([])); render(Agenda);
  await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(1));
  expect(h.agenda.mock.calls[0]).toEqual(['2026-10-30T04:00:00.000Z', '2026-11-06T05:00:00.000Z', 'America/New_York']);
  expect(screen.getByRole('button', { name: 'Week' })).toHaveAttribute('aria-pressed', 'true');
  expect(document.querySelectorAll('[data-day]')).toHaveLength(7);
  expect(within(daySection('2026-10-30')).getByRole('heading')).toHaveTextContent(/Friday.*30.*Today/);
  expect(await screen.findAllByText('Nothing on this day.')).toHaveLength(7);
  await fireEvent.click(screen.getByRole('button', { name: 'Next week' }));
  await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(2));
  expect(h.agenda.mock.calls[1]).toEqual(['2026-11-06T05:00:00.000Z', '2026-11-13T05:00:00.000Z', 'America/New_York']);
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
  expect(screen.queryByRole('dialog')).toBeNull(); await screen.findByText('Occurrence moved to November 6, 2026, 09:00. Its reminder follows the target.');
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
  const d = await screen.findByRole('dialog'); await fireEvent.click(within(d).getByRole('checkbox')); await fireEvent.click(within(d).getByRole('button', { name: 'Move', exact: true }));
  await waitFor(() => expect(h.move).toHaveBeenCalledWith('work', 'CH-2', expect.objectContaining({ role: 'target', acknowledge_violations: true })));
 });
 it.each(['different', 'missing'])('rejects a %s preview root and reloads without a write', async kind => {
  h.preview.mockResolvedValue(preview(undefined, { changes: kind === 'missing' ? [] : [{ ...preview().changes[0], from: '2026-11-04T14:00:00Z' }] }));
  await mount(); await drop(); await screen.findByText('This occurrence changed. Review the updated entry and try again.');
  expect(h.move).not.toHaveBeenCalled(); await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(2));
 });
 it.each(['preview', 'move'])('handles commitment_changed from %s without retry', async source => {
  h[source].mockRejectedValue({ code: 'commitment_changed' }); await mount(); await drop();
  await screen.findByText('This occurrence changed. Review the updated entry and try again.');
  expect(h.move).toHaveBeenCalledTimes(source === 'move' ? 1 : 0); expect(h.preview).toHaveBeenCalledTimes(1);
  await waitFor(() => expect(h.agenda).toHaveBeenCalledTimes(2));
 });
 it('reviews every change, requires a deadline reason and lists finished dates and role labels', async () => {
  const linked = { item_id: 'linked', role: 'earliest' as const, to: '2026-11-07T15:00:00Z' };
  h.preview.mockResolvedValue(preview(undefined, { renegotiates_deadline: true, changes: [linked, ...preview().changes], left_in_place: [{ ...linked, role: 'occupied_until' }] }));
  await mount(); await drag(); await screen.findByText('Review needed: this move changes a hard deadline.'); await fireEvent.drop(daySection());
  const d = await screen.findByRole('dialog', { name: 'Review deadline change' });
  expect(within(d).getByText('This move changes a hard deadline.')).toBeTruthy();
  expect(within(d).getByText(/First item → November 6/)).toBeTruthy();
  expect(within(d).getByText(/Linked child \(linked\).*Available from/)).toBeTruthy();
  expect(within(d).getByText('Not moved (finished)')).toBeTruthy();
  await fireEvent.click(within(d).getByRole('button', { name: 'Move', exact: true }));
  expect(within(d).getByText('Enter a reason.')).toBeTruthy(); expect(h.move).not.toHaveBeenCalled();
  await fireEvent.input(within(d).getByLabelText('Reason'), { target: { value: ' Agreed later ' } });
  await fireEvent.click(within(d).getByRole('button', { name: 'Move', exact: true }));
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
  expect(await within(d).findByText(/Linked item \(linked\)/)).toBeTruthy();
  expect(within(d).getByText('Outside its bound')).toBeTruthy();
  await fireEvent.click(within(d).getByRole('button', { name: 'Move', exact: true })); expect(h.move).not.toHaveBeenCalled();
  await fireEvent.click(within(d).getByLabelText('Accept dates outside their bounds'));
  await fireEvent.click(within(d).getByRole('button', { name: 'Move', exact: true }));
  await waitFor(() => expect(h.move).toHaveBeenCalledWith('work', 'EX-30', { role: 'target', to: '2026-11-06T14:00:00.000Z', revision: 'revision-1', acknowledge_violations: true }));
 });
 it('pins available and deadline days while dragging and clears drag decorations on Escape', async () => {
  const roles = { ...row().roles!, target: row().at, earliest: '2026-11-06T14:00:00Z', deadline: '2026-11-08T14:00:00Z' };
  h.agenda.mockResolvedValue(response([row({ roles, roles_origin: 'native' })])); await mount(); await drag();
  expect(within(daySection()).getByText('Available from', { exact: true })).toBeTruthy();
  expect(within(daySection('2026-11-08')).getByText('Deadline', { exact: true })).toBeTruthy();
  await fireEvent.keyDown(window, { key: 'Escape' });
  expect(screen.queryByText('Available from', { exact: true })).toBeNull(); expect(h.move).not.toHaveBeenCalled();
  await fireEvent.drop(daySection()); expect(h.move).not.toHaveBeenCalled();
 });
 it('labels target and all date roles, with noninteractive separate-day deadline markers', async () => {
  h.agenda.mockResolvedValue(response([row({ roles: { ...row().roles!, target: row().at, earliest: '2026-11-05T13:00:00Z', deadline: '2026-11-05T16:00:00Z', deadline_behavior: 'stays_owed', occupied_until: '2026-11-05T15:00:00Z' } }), row({ item_ref: 'EX-31', item_title: 'Another item', roles: { ...row().roles!, deadline: '2026-11-08T14:00:00Z', deadline_behavior: 'lapses' } })]));
  await mount(); const day = within(daySection('2026-11-05'));
  expect(day.getAllByText(/Target.*09:00/).length).toBeGreaterThan(0);
  expect(day.getByText(/Deadline.*11:00.*Still owed/)).toBeTruthy();
  expect(day.getByText(/Available from.*08:00/)).toBeTruthy(); expect(day.getByText('Until 10:00')).toBeTruthy();
  const marker = within(daySection('2026-11-08')).getByText('Deadline · Another item').closest('li')!;
  expect(within(marker).queryByRole('button')).toBeNull(); expect(marker.getAttribute('draggable')).not.toBe('true');
  expect(within(daySection('2026-11-08')).getByText('Nothing on this day.')).toBeTruthy();
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
  expect(within(daySection('2026-11-08')).getByText('Deadline · First item')).toBeTruthy();
  expect(within(daySection('2026-11-07')).queryByText(/Deadline ·/)).toBeNull();
  expect(within(daySection('2026-11-05')).getByText(/Deadline November 8, 2026 · Lapses/)).toBeTruthy();
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
 it('Week says when the created and completed toggle hides every entry', async () => {
  localStorage.setItem('pad-agenda-show-events:user-a', '0');
  h.agenda.mockResolvedValue(response([row({ type: 'created', item_title: 'Created item' })]));
  render(Agenda);
  expect(await screen.findByText('Created and completed entries are hidden.')).toBeTruthy();
  await fireEvent.click(screen.getByRole('button', { name: 'Show created and completed' }));
  expect(await screen.findByText('Created item')).toBeTruthy();
 });
 it('deadline markers come only from live occurrences, never history rows', async () => {
  const roles = { ...row().roles!, deadline: '2026-11-08T14:00:00Z', deadline_behavior: 'lapses' as const };
  h.agenda.mockResolvedValue(response([
   row({ item_ref: 'EX-31', item_title: 'Another item', roles }),
   row({ item_ref: 'EX-31', item_title: 'Another item', roles, state: 'moved', at: '2026-11-04T14:00:00Z', day: '2026-11-04', wall: '2026-11-04T09:00:00' }),
   row({ item_ref: 'EX-32', item_title: 'Finished item', roles, state: 'done', at: '2026-11-06T14:00:00Z', day: '2026-11-06', wall: '2026-11-06T09:00:00' })
  ]));
  render(Agenda); await screen.findAllByText('Another item');
  const day = within(daySection('2026-11-08'));
  expect(day.getAllByText('Deadline · Another item')).toHaveLength(1);
  expect(day.queryByText('Deadline · Finished item')).toBeNull();
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
 it('renders ranks in server order, reason chips, group completeness and target/deadline text', async () => {
  h.agenda.mockResolvedValue(response([], { priority_list: [workRow({ item_title: 'High B', rank: 'high', reason: 'target_passed' }), workRow({ item_ref: 'EX-31', item_title: 'High A', rank: 'high', reason: 'deadline_still_owed' }), workRow({ item_ref: 'EX-32', item_title: 'Normal row', roles: { ...row().roles!, target: row().at, deadline: '2026-11-06T14:00:00Z' } }), workRow({ item_ref: 'EX-33', item_title: 'Low row', rank: 'low' }), workRow({ item_ref: 'EX-34', item_title: 'Unranked row', rank: undefined })], waiting_on: [workRow({ type: 'item', item_ref: 'W-1', item_title: 'Blocked work', reason: 'blocked_needs_decision' }), workRow({ type: 'item', item_ref: 'W-2', item_title: 'Waiting work', reason: 'waiting' })], no_date_complete: false })); render(Agenda);
  const list = await screen.findByRole('region', { name: 'Priority' });
  expect(within(list).getAllByRole('heading').map(el => el.textContent)).toEqual(['Priority', 'High', 'Normal', 'Low', 'Unranked', 'Waiting on', 'No date']);
  expect(within(list).getAllByRole('link').slice(0, 2).map(el => el.textContent)).toEqual(['High B', 'High A']);
  for (const text of ['Showing all outstanding', 'Target passed', 'Missed deadline · Still owed', 'Needs a decision', 'This list is incomplete.']) expect(within(list).getByText(text)).toBeTruthy();
  expect(within(list).getByText(/Target November 5.*09:00/)).toBeTruthy(); expect(within(list).getByText(/Deadline November 6.*09:00/)).toBeTruthy();
  expect(within(list).queryByText('No undated work.')).toBeNull(); expect(within(list).queryByRole('button', { name: /^Done/ })).toBeNull();
  expect(screen.queryByText('Agenda is incomplete.')).toBeNull();
 });
 it('renders complete empty groups and keeps window incompleteness independent', async () => {
  h.agenda.mockResolvedValue(response([], { window_complete: false })); render(Agenda);
  for (const text of ['Nothing outstanding.', 'Nothing waiting.', 'No undated work.', 'Agenda is incomplete.']) expect(await screen.findByText(text)).toBeTruthy();
 });
 it('never calls an incomplete empty group empty', async () => {
  h.agenda.mockResolvedValue(response([], { priority_list_complete: false, waiting_on_complete: false, no_date_complete: false })); render(Agenda);
  await waitFor(() => expect(screen.getAllByText('This list is incomplete.')).toHaveLength(3));
  for (const text of ['Nothing outstanding.', 'Nothing waiting.', 'No undated work.']) expect(screen.queryByText(text)).toBeNull();
 });
 it('moves list schedules through their own target and ref, without Done', async () => {
  h.agenda.mockResolvedValue(response([], { priority_list: [workRow({ day: undefined, at: undefined, roles: { ...row().roles!, target: row().at } })] })); render(Agenda);
  await screen.findByRole('region', { name: 'Priority' });
  await fireEvent.click(screen.getByRole('button', { name: 'Move target…' }));
  const d = screen.getByRole('dialog', { name: 'Move target' }); await fillTime(d, '2026-11-06T09:00');
  await fireEvent.click(within(d).getByRole('button', { name: 'Move target' }));
  await waitFor(() => expect(h.move).toHaveBeenCalledWith('work', 'EX-30', { role: 'target', to: '2026-11-06T14:00:00.000Z', revision: 'revision-1' }));
 });
 it('switches between Agenda and List in place at mobile widths without fetching', async () => {
  vi.spyOn(window, 'matchMedia').mockImplementation(query => ({ matches: query === '(max-width: 768px)', media: query, addEventListener: vi.fn(), removeEventListener: vi.fn() }) as unknown as MediaQueryList);
  await mount();
  expect(screen.getByRole('button', { name: 'Agenda', exact: true })).toHaveAttribute('aria-pressed', 'true');
  await fireEvent.click(screen.getByRole('button', { name: 'List', exact: true }));
  expect(screen.getByRole('region', { name: 'Priority' })).toBeVisible(); expect(screen.queryByText('First item')).toBeNull();
  await fireEvent.click(screen.getByRole('button', { name: 'Agenda', exact: true })); await screen.findByText('First item'); expect(h.agenda).toHaveBeenCalledTimes(1);
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
  h.move.mockRejectedValue({ code, message: 'Server explanation' }); await mount(); await drop(); await screen.findByText(copy);
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
  await screen.findByText(/Occurrence moved to/);
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
  const d=await screen.findByRole('dialog',{name:'Forecast'});await within(d).findByText(/No children selected/);
  expect((within(d).getByLabelText('Date') as HTMLInputElement).value).toBe('');
  await fireEvent.input(within(d).getByLabelText('Date'),{target:{value:'2026-11-10'}});
  await fireEvent.click(within(d).getByRole('button',{name:'Save'}));
  await waitFor(()=>expect(h.setForecast).toHaveBeenCalledWith('work','EX-30',{value:'2026-11-10'}));
  await waitFor(()=>expect(h.agenda).toHaveBeenCalledTimes(2));
  expect(screen.queryByText('Loading agenda…')).toBeNull();
 });
 it('opens Forecast from the priority list and clears the estimate',async()=>{
  h.agenda.mockResolvedValue(response([row()],{priority_list:[workRow({roles:{...row().roles!,target:row().at},forecast:{value:'2026-11-10',set_by:'u1',set_by_kind:'agent',set_at:'now',source:''}})]}));
  h.view.mockResolvedValue({forecast:{value:'2026-11-10',set_by:'u1',set_by_kind:'agent',set_at:'now',source:'',passed:false},milestone});
  h.clearForecast.mockResolvedValue({});
  render(Agenda);await screen.findByRole('button',{name:'Forecast…'});await fireEvent.click(screen.getByRole('button',{name:'Forecast…'}));
  const d=await screen.findByRole('dialog',{name:'Forecast'});await within(d).findByRole('button',{name:'Clear forecast'});
  await fireEvent.click(within(d).getByRole('button',{name:'Clear forecast'}));
  await waitFor(()=>expect(h.clearForecast).toHaveBeenCalledWith('work','EX-30'));
  await waitFor(()=>expect(h.agenda).toHaveBeenCalledTimes(2));
 });
});
