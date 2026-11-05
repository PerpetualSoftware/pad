import { beforeEach, afterEach, describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent, waitFor, cleanup, within } from '@testing-library/svelte';
import { flushSync } from 'svelte';
import type { AgendaEntry, AgendaResponse } from '$lib/types';

const h = vi.hoisted(() => ({ agenda: vi.fn(), view: vi.fn(), resolve: vi.fn(), user: { id: 'user-a', timezone: 'America/New_York' } as { id: string; timezone?: string } | null, epoch: 0 }));
vi.mock('$lib/api/client', () => ({ api: { getMyAgenda: h.agenda, getSchedule: h.view, resolveSchedule: h.resolve } }));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { get user() { return h.user; }, get identityEpoch() { return h.epoch; }, identityFence() { const epoch = h.epoch; return () => epoch === h.epoch; } } }));
import Agenda from './+page.svelte';

const row = (extra: Partial<AgendaEntry> = {}): AgendaEntry => ({ type: 'schedule', at: '2026-11-05T14:00:00Z', day: '2026-11-05', wall: '2026-11-05T09:00:00', floating: true, item_ref: 'EX-30', item_title: 'First item', collection: 'tasks', workspace: 'work', workspace_name: 'First workspace', owner_username: 'alice', state: 'outstanding', kind: 'deadline_soft', schedule_id: 'schedule-1', ...extra });
const frozenAt = '2026-11-05T17:00:00Z';
const response = (entries: AgendaEntry[] = [row()], extra: Partial<AgendaResponse> = {}): AgendaResponse => ({ from: '', to: '', tz: 'America/New_York', entries, ...extra });
const deferred = <T,>() => { let resolve!: (value: T) => void; const promise = new Promise<T>(r => { resolve = r; }); return { promise, resolve }; };
const settle = async () => { await new Promise(r => setTimeout(r, 0)); flushSync(); };
async function mount() { render(Agenda); await screen.findByText('First item'); }
async function menu(ref = 'EX-30') { await fireEvent.click(screen.getByRole('button', { name: `Actions ${ref}` })); }
async function dialog(kind: 'Move' | 'Snooze') {
 await menu();
 await fireEvent.click(screen.getByRole('menuitem', { name: `${kind}…` }));
 return screen.getByRole('dialog', { name: `${kind} occurrence` });
}
async function fillTime(d: HTMLElement, value = '2026-11-06T09:30') { await fireEvent.input(within(d).getByLabelText(/New date and time|Remind me again at/), { target: { value } }); }

beforeEach(() => {
 vi.resetAllMocks(); localStorage.clear(); h.user = { id: 'user-a', timezone: 'America/New_York' }; h.epoch = 0;
 vi.spyOn(Date, 'now').mockReturnValue(Date.parse('2026-11-05T12:00:00Z'));
 h.agenda.mockResolvedValue(response());
 h.view.mockResolvedValue({ item_ref: 'EX-30', kind: 'deadline_soft', floating: true, next_occurrence: { at: frozenAt, wall: row().wall } });
 h.resolve.mockResolvedValue({ schedule: {}, resolution: { occurrence_at: frozenAt, state: 'done', missed_recorded: 0 } });
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); });

describe('agenda Day page', () => {
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
 it('offers controls only on outstanding schedules and disables hard-deadline Move', async () => {
  const entries = ['scheduled', 'done', 'skipped', 'moved', 'missed', 'unrecorded'].map((state, i) => row({ state: state as AgendaEntry['state'], item_ref: `EX-${i}`, schedule_id: `s${i}` }));
  h.agenda.mockResolvedValue(response([...entries, ...['date', 'created', 'completed'].map(type => row({ type: type as AgendaEntry['type'] })), row({ kind: 'deadline_hard' })]));
  render(Agenda); await screen.findByRole('button', { name: 'Done EX-30' }); expect(screen.getAllByRole('button', { name: /^Done / })).toHaveLength(1);
  await menu(); expect((screen.getByRole('menuitem', { name: 'Hard deadlines cannot be moved.' }) as HTMLButtonElement).disabled).toBe(true);
 });
});

describe('resolve identity and dialogs', () => {
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
 it('opens Move without fetching and rejects a whitespace-only reason', async () => {
  await mount(); const d = await dialog('Move'); await fillTime(d); await fireEvent.input(within(d).getByLabelText('Reason'), { target: { value: '  ' } });
  await fireEvent.click(within(d).getByRole('button', { name: 'Move occurrence' })); expect(within(d).getByText('Enter a reason.')).toBeTruthy();
  expect(within(d).getByLabelText('Reason').getAttribute('aria-invalid')).toBe('true'); expect(within(d).getByLabelText('Reason').getAttribute('aria-describedby')).toBeTruthy();
  expect(h.view).not.toHaveBeenCalled(); expect(h.resolve).not.toHaveBeenCalled();
 });
 it('Move sends the shown zone instant and reason with the frozen identity', async () => {
  await mount(); const d = await dialog('Move'); expect(h.view).not.toHaveBeenCalled(); await fillTime(d);
  expect(within(d).getByText(/Will be.*09:30.*GMT-05:00/)).toBeTruthy();
  await fireEvent.input(within(d).getByLabelText('Reason'), { target: { value: 'Waiting for review' } });
  await fireEvent.click(within(d).getByRole('button', { name: 'Move occurrence' }));
  await waitFor(() => expect(h.resolve).toHaveBeenCalledWith('work', 'EX-30', { occurrence_at: frozenAt, state: 'move', until: '2026-11-06T14:30:00.000Z', comment: 'Waiting for review' })); await screen.findByText('Occurrence moved.');
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
