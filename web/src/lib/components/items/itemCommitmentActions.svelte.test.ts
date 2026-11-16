import { afterEach, beforeEach, expect, it, vi } from 'vitest';
const h = vi.hoisted(() => ({ get: vi.fn(), resolve: vi.fn(), preview: vi.fn(), move: vi.fn(), item: vi.fn(), update: vi.fn(), auth: { identityEpoch: 0 } }));
vi.mock('$lib/api/client', () => ({ api: { getCommitment: h.get, resolveCommitment: h.resolve, previewCommitmentMove: h.preview, moveCommitment: h.move, items: { get: h.item, update: h.update } } }));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { get identityEpoch() { return h.auth.identityEpoch; }, identityFence() { const epoch = h.auth.identityEpoch; return () => epoch === h.auth.identityEpoch; } } }));
import { createItemCommitmentActions } from './itemCommitmentActions.svelte';
import { commitmentAgendaEntry } from '$lib/commitment';
import { collection, item, otherId, view } from '$lib/test/commitmentFixtures';
import type { AgendaEntry, ItemCommitmentView } from '$lib/types';

const staleCopy = 'This occurrence changed. Review the updated entry and try again.';
let identity = { wsSlug: 'work', itemSlug: 'TASK-1', itemId: item.id };
let refresh: ReturnType<typeof vi.fn>;
const make = () => createItemCommitmentActions({ identity: () => identity, zone: () => 'UTC', refresh: () => refresh() });
const entryOf = (v: ItemCommitmentView = view()) => commitmentAgendaEntry(v, { wsSlug: 'work', item, collection, zone: 'UTC' }) as AgendaEntry;
const flush = async () => { for (let i = 0; i < 10; i++) await Promise.resolve(); };
function deferred<T>() { let resolve!: (v: T) => void; const promise = new Promise<T>(r => { resolve = r; }); return { promise, resolve }; }
const resolved = (extra = {}) => ({ commitment: view(), resolution: { occurrence_at: view().next_occurrence!.at, state: 'done', missed_recorded: 0, ...extra } });
const root = (from = '2031-03-10T00:00:00Z', to = '2031-03-12T00:00:00.000Z') => ({ item_id: item.id, role: 'target' as const, from, to, root: true });
beforeEach(() => {
 vi.resetAllMocks(); h.auth.identityEpoch = 0; identity = { wsSlug: 'work', itemSlug: 'TASK-1', itemId: item.id };
 refresh = vi.fn().mockResolvedValue(true);
 h.get.mockResolvedValue(view()); h.resolve.mockResolvedValue(resolved()); h.item.mockResolvedValue({ ...item, id: otherId, title: 'Prep' });
 vi.spyOn(Date, 'now').mockReturnValue(Date.parse('2031-03-10T12:00:00Z'));
});
afterEach(() => vi.restoreAllMocks());

it('refuses a stale occurrence after a fresh read and refreshes instead of resolving', async () => {
 const c = make(); h.get.mockResolvedValueOnce(view({ next_occurrence: { at: '2031-03-16T10:30:00Z', wall: '2031-03-17T00:30:00', state: 'outstanding' } }));
 c.action(entryOf(), 'done'); await flush();
 expect(h.resolve).not.toHaveBeenCalled(); expect(c.actionError).toBe(staleCopy); expect(refresh).toHaveBeenCalledTimes(1);
});
it('refuses the other occurrence of a repeated DST wall', async () => {
 const c = make();
 const shown = view({ occurrence_key: '2031-11-02T05:30:00Z|k', next_occurrence: { at: '2031-11-02T05:30:00Z', wall: '2031-11-02T01:30:00', state: 'outstanding' } });
 h.get.mockResolvedValueOnce({ ...shown, next_occurrence: { ...shown.next_occurrence!, at: '2031-11-02T06:30:00Z' } });
 c.action(entryOf(shown), 'done'); await flush();
 expect(h.resolve).not.toHaveBeenCalled(); expect(c.actionError).toBe(staleCopy);
});
it('sends one resolve for a double click, freezes the occurrence and announces once', async () => {
 const c = make(); const entry = entryOf();
 c.action(entry, 'done'); c.action(entry, 'done'); await flush();
 expect(h.resolve).toHaveBeenCalledTimes(1);
 expect(h.resolve).toHaveBeenCalledWith('work', 'TASK-1', { occurrence_at: '2031-03-09T10:30:00Z', state: 'done' });
 expect(c.announcement).toBe('Occurrence marked done.'); expect(refresh).toHaveBeenCalledTimes(1);
 c.action(entry, 'skip'); await flush(); expect(h.resolve).toHaveBeenCalledTimes(1);
});
it('snoozes through the dialog with the chosen instant', async () => {
 const c = make(); const entry = entryOf();
 c.action(entry, 'snooze'); expect(c.dialogMode).toBe('snooze'); expect(c.dialogEntry).toEqual(entry);
 c.submitDialog('2031-03-11T09:00:00.000Z'); await flush();
 expect(h.resolve).toHaveBeenCalledWith('work', 'TASK-1', { occurrence_at: '2031-03-09T10:30:00Z', state: 'snooze', until: '2031-03-11T09:00:00.000Z' });
 expect(c.dialogEntry).toBeNull(); expect(c.announcement).toBe('Occurrence snoozed.');
});
it('asks how to end the last occurrence, then reports a failed item close', async () => {
 const c = make();
 h.resolve.mockRejectedValueOnce({ code: 'item_still_open', details: { occurrence_at: 'x', done_field: 'status', item_status: 'open', close_options: ['shipped'], ack_available: false } });
 c.action(entryOf(), 'done'); await flush();
 expect(c.endChoice?.details).toMatchObject({ state: 'done', close_options: ['shipped'] });
 h.resolve.mockResolvedValueOnce(resolved({ item_still_open: true })); h.update.mockRejectedValueOnce({ message: 'locked' });
 await c.chooseEnd({ kind: 'close', value: 'shipped' });
 expect(h.resolve).toHaveBeenLastCalledWith('work', 'TASK-1', { occurrence_at: '2031-03-09T10:30:00Z', state: 'done', end_schedule: true });
 expect(h.update).toHaveBeenCalledWith('work', 'TASK-1', { fields_patch: { status: 'shipped' } });
 expect(c.actionError).toBe('The schedule has ended, but closing the item failed: locked. It stays open in Outstanding.');
 expect(c.endChoice).toBeNull();
});
it('moves after checking the preview root and sends its revision', async () => {
 const c = make();
 h.preview.mockResolvedValueOnce({ revision: 'r1', changes: [root()] }); h.move.mockResolvedValueOnce({ revision: 'r2', changes: [root()] });
 c.action(entryOf(), 'move'); c.submitDialog('2031-03-12T00:00:00.000Z', 'later'); await flush();
 expect(h.preview).toHaveBeenCalledWith('work', 'TASK-1', { role: 'target', to: '2031-03-12T00:00:00.000Z', reason: 'later' });
 expect(h.move).toHaveBeenCalledWith('work', 'TASK-1', { role: 'target', to: '2031-03-12T00:00:00.000Z', revision: 'r1', reason: 'later' });
 expect(c.announcement).toMatch(/^Occurrence moved to /);
});
it('refuses a move whose preview starts from another target', async () => {
 const c = make(); h.preview.mockResolvedValueOnce({ revision: 'r1', changes: [root('2031-03-11T00:00:00Z')] });
 c.action(entryOf(), 'move'); c.submitDialog('2031-03-12T00:00:00.000Z'); await flush();
 expect(h.move).not.toHaveBeenCalled(); expect(c.actionError).toBe(staleCopy); expect(refresh).toHaveBeenCalled();
});
it('opens review for a renegotiated deadline and commits from it', async () => {
 const c = make();
 h.preview.mockResolvedValueOnce({ revision: 'r1', renegotiates_deadline: true, changes: [root(), { item_id: otherId, role: 'deadline', to: '2031-03-13' }] });
 c.action(entryOf(), 'move'); c.submitDialog('2031-03-12T00:00:00.000Z'); await flush();
 expect(c.review?.preview.revision).toBe('r1'); expect(h.move).not.toHaveBeenCalled(); expect(c.linkedTitles[otherId]).toBe('Prep');
 h.move.mockResolvedValueOnce({ revision: 'r2', changes: [root()] });
 await c.commitReview('because', true);
 expect(h.move).toHaveBeenCalledWith('work', 'TASK-1', { role: 'target', to: '2031-03-12T00:00:00.000Z', revision: 'r1', reason: 'because', acknowledge_violations: true });
 expect(c.review).toBeNull();
});
it('re-previews into review when the move lands after the deadline', async () => {
 const c = make();
 h.preview.mockResolvedValueOnce({ revision: 'r1', changes: [root()] }).mockResolvedValueOnce({ revision: 'r3', past_deadline: '2031-03-11', changes: [root()] });
 h.move.mockRejectedValueOnce({ code: 'target_after_deadline', message: 'After the deadline.' });
 c.action(entryOf(), 'move'); c.submitDialog('2031-03-12T00:00:00.000Z'); await flush();
 expect(h.preview).toHaveBeenCalledTimes(2); expect(c.review?.preview.revision).toBe('r3'); expect(c.actionError).toBe('After the deadline.');
});
it('writes nothing after the account or the item identity changes', async () => {
 const c = make(); const read = deferred<ItemCommitmentView>(); h.get.mockReturnValueOnce(read.promise);
 c.action(entryOf(), 'done'); await flush(); h.auth.identityEpoch++; read.resolve(view()); await flush();
 expect(h.resolve).not.toHaveBeenCalled();
 h.auth.identityEpoch = 0; const c2 = make(); const read2 = deferred<ItemCommitmentView>(); h.get.mockReturnValueOnce(read2.promise);
 c2.action(entryOf(), 'done'); await flush(); identity = { ...identity, itemSlug: 'TASK-2', itemId: otherId }; read2.resolve(view()); await flush();
 expect(h.resolve).not.toHaveBeenCalled();
});
it('writes nothing after disposal', async () => {
 const c = make(); const read = deferred<ItemCommitmentView>(); h.get.mockReturnValueOnce(read.promise);
 c.action(entryOf(), 'done'); await flush(); c.dispose(); read.resolve(view()); await flush();
 expect(h.resolve).not.toHaveBeenCalled();
});
it('reports a failed refresh after a saved write, and Retry only re-reads', async () => {
 const c = make(); refresh.mockResolvedValueOnce(false);
 c.action(entryOf(), 'done'); await flush();
 expect(c.refreshFailed).toBe(true); expect(c.announcement).toBe('Occurrence marked done.');
 await c.retryRefresh();
 expect(refresh).toHaveBeenCalledTimes(2); expect(h.resolve).toHaveBeenCalledTimes(1); expect(c.refreshFailed).toBe(false);
});
it('opens the forecast dialog and refreshes after a save', async () => {
 const c = make(); const entry = entryOf();
 c.action(entry, 'forecast'); expect(c.forecastEntry).toEqual(entry);
 c.forecastSaved(); expect(c.forecastEntry).toBeNull(); expect(refresh).toHaveBeenCalledTimes(1);
});
it('brings the target to today through the move flow', async () => {
 const c = make();
 h.preview.mockResolvedValueOnce({ revision: 'r1', changes: [root('2031-03-08T00:00:00Z', '2031-03-10T00:00:00.000Z')] });
 c.action(entryOf(view({ roles: { ...view().roles, target: '2031-03-08' } })), 'today'); await flush();
 expect(h.preview).toHaveBeenCalledWith('work', 'TASK-1', { role: 'target', to: '2031-03-10T00:00:00.000Z' });
 expect(h.move).toHaveBeenCalledWith('work', 'TASK-1', expect.objectContaining({ to: '2031-03-10T00:00:00.000Z', revision: 'r1' }));
});
