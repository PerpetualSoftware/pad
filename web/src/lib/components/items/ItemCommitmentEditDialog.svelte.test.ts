import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
const h = vi.hoisted(() => ({ get: vi.fn(), update: vi.fn(), reneg: vi.fn(), calls: [] as string[], auth: { identityEpoch: 0, user: { timezone: 'America/New_York' } } }));
vi.mock('$lib/api/client', () => ({ api: {
 getCommitment: (...a: unknown[]) => { h.calls.push('get'); return h.get(...a); },
 updateCommitment: (...a: unknown[]) => { h.calls.push('patch'); return h.update(...a); },
 renegotiateCommitment: (...a: unknown[]) => { h.calls.push((a[2] as { preview?: boolean }).preview ? 'preview' : 'apply'); return h.reneg(...a); }
} }));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { get identityEpoch() { return h.auth.identityEpoch; }, get user() { return h.auth.user; }, identityFence() { const epoch = h.auth.identityEpoch; return () => epoch === h.auth.identityEpoch; } } }));
import ItemCommitmentEditDialog from './ItemCommitmentEditDialog.svelte';
import { view } from '$lib/test/commitmentFixtures';
import type { ItemCommitmentView } from '$lib/types';
import { roleText } from '$lib/calendar/presentation';

const base = { target: '2031-03-10T13:00:00Z', earliest: null, deadline: null, deadline_behavior: null, occupied_until: null, outcome: '', roles_origin: 'native' } as const;
const offsets = { target_days: 0, target_secs: 0, earliest_days: null, earliest_secs: null, deadline_days: null, deadline_secs: null, occupied_days: null, occupied_secs: null };
const plain = () => view({ dtstart: '2031-03-10T09:00:00', tz: 'America/New_York', rrule: 'FREQ=WEEKLY', roles: { ...base }, role_offsets: { ...offsets }, updated_at: '2031-03-01T00:00:00Z', next_occurrence: { at: '2031-03-10T13:00:00Z', wall: '2031-03-10T09:00:00', state: 'outstanding' } });
const native = () => ({ ...plain(), roles: { ...base, deadline: '2031-03-11T05:00:00Z', deadline_behavior: 'stays_owed' as const }, role_offsets: { ...offsets, deadline_days: 1, deadline_secs: -28800 } });
const previewResult = (revision: string, extra = {}) => ({ revision, preview: { scope: 'series', occurrence_key: 'k', applies_from: '2031-03-10T13:00:00Z|2031-03-10T09:00:00', current_deadline: '2031-03-11T05:00:00Z', new_deadline: '2031-03-12T05:00:00Z', current_deadline_behavior: 'stays_owed', new_deadline_behavior: 'stays_owed', current_offset: null, new_offset: null, miss_recorded_first: false, miss_occurrence_key: null, lapses_on_apply: false, keeps_deadline_override: false, clears_deadline_overrides: false }, ...extra });
function deferred<T>() { let resolve!: (v: T) => void; const promise = new Promise<T>(a => { resolve = a; }); return { promise, resolve }; }
const mount = (v: ItemCommitmentView) => {
 const onsaved = vi.fn(), onclose = vi.fn(), onreload = vi.fn(), onloadtitles = vi.fn();
 render(ItemCommitmentEditDialog, { open: true, view: v, wsSlug: 'work', itemSlug: 'TASK-1', itemTitle: 'Work', zone: 'America/New_York', mobile: false, onsaved, onclose, onreload, onloadtitles, titles: { other: 'Prep' } });
 return { onsaved, onreload, onloadtitles };
};
const input = (label: string, value: string) => fireEvent.input(screen.getByLabelText(label), { target: { value } });
const save = () => fireEvent.click(screen.getByRole('button', { name: 'Save' }));
beforeEach(() => { vi.resetAllMocks(); h.calls.length = 0; h.auth.identityEpoch = 0; h.update.mockResolvedValue({}); vi.spyOn(Date, 'now').mockReturnValue(Date.parse('2031-03-01T12:00:00Z')); });
afterEach(() => { cleanup(); vi.restoreAllMocks(); });

it('fences on a fresh GET, then PATCHes only the changed member', async () => {
 h.get.mockResolvedValue(plain());
 const { onsaved } = mount(plain());
 await input('Target date', '2031-03-17'); await save();
 await waitFor(() => expect(onsaved).toHaveBeenCalledTimes(1));
 expect(h.calls).toEqual(['get', 'patch']);
 expect(h.update).toHaveBeenCalledWith('work', 'TASK-1', { dtstart: '2031-03-17T09:00:00' });
});
it('makes no request for an untouched form', async () => {
 mount(plain()); await save();
 expect(await screen.findByText('No changes.')).toBeTruthy();
 expect(h.calls).toEqual([]);
});
it('writes nothing when the stored date changed since the dialog opened', async () => {
 h.get.mockResolvedValue({ ...plain(), updated_at: '2031-03-01T00:00:05Z' });
 const { onsaved, onreload } = mount(plain());
 await input('Target date', '2031-03-17'); await save();
 expect(await screen.findByText(/This date changed\. Reload to edit the current values\./)).toBeTruthy();
 expect(h.calls).toEqual(['get']); expect(onsaved).not.toHaveBeenCalled();
 await fireEvent.click(screen.getByRole('button', { name: 'Reload' })); expect(onreload).toHaveBeenCalledTimes(1);
});
it('locks start and Repeats while a native deadline exists', async () => {
 mount(native());
 expect((screen.getByLabelText('Target date') as HTMLInputElement).disabled).toBe(true);
 expect((screen.getByLabelText('Repeats') as HTMLSelectElement).disabled).toBe(true);
 expect(screen.getByText('Fixed while a hard deadline exists; renegotiate or remove the deadline first.')).toBeTruthy();
});
it('changes a native deadline through preview then apply, with no PATCH', async () => {
 h.get.mockResolvedValue(native()); h.reneg.mockResolvedValueOnce(previewResult('rev1', { cascade: { revision: 'c', changes: [{ item_id: 'other', role: 'target', to: '2031-03-13T13:00:00Z' }] } })).mockResolvedValueOnce(native());
 const { onsaved, onloadtitles } = mount(native());
 expect((screen.getByLabelText('Days after the target') as HTMLInputElement).value).toBe('1');
 expect((screen.getByLabelText('Deadline time') as HTMLInputElement).value).toBe('01:00');
 await input('Days after the target', '2');
 await save();
 expect(await screen.findByText('Give a reason for changing the deadline.')).toBeTruthy(); expect(h.calls).toEqual([]);
 await input('Reason for the deadline change', 'Client asked'); await save();
 await screen.findByRole('button', { name: 'Change deadline' });
 expect(screen.getByText(/Prep: target moves to/)).toBeTruthy(); expect(onloadtitles).toHaveBeenCalledTimes(1);
 expect(h.reneg).toHaveBeenNthCalledWith(1, 'work', 'TASK-1', { scope: 'series', deadline_offset: { deadline_days: 2, deadline_secs: -28800 }, reason: 'Client asked', preview: true });
 await fireEvent.click(screen.getByRole('button', { name: 'Change deadline' }));
 await waitFor(() => expect(onsaved).toHaveBeenCalledTimes(1));
 expect(h.reneg).toHaveBeenNthCalledWith(2, 'work', 'TASK-1', { scope: 'series', deadline_offset: { deadline_days: 2, deadline_secs: -28800 }, reason: 'Client asked', revision: 'rev1' });
 expect(h.calls).toEqual(['get', 'preview', 'apply']);
});
it('acknowledges violations the preview reported and removes a deadline', async () => {
 h.get.mockResolvedValue(native()); h.reneg.mockResolvedValueOnce(previewResult('rev1', { cascade: { revision: 'c', changes: [], violations: [{ item_id: 'other', role: 'deadline', to: '2031-03-14T13:00:00Z' }] } })).mockResolvedValueOnce(native());
 const { onsaved } = mount(native());
 await fireEvent.click(screen.getByLabelText('Hard deadline'));
 await input('Reason for the deadline change', 'Not needed'); await save();
 await screen.findByText(/Prep: deadline lands outside its own bounds/);
 await fireEvent.click(screen.getByRole('button', { name: 'Change deadline' }));
 await waitFor(() => expect(onsaved).toHaveBeenCalledTimes(1));
 expect(h.reneg).toHaveBeenNthCalledWith(1, 'work', 'TASK-1', { scope: 'series', remove_deadline: true, reason: 'Not needed', preview: true });
 expect(h.reneg).toHaveBeenNthCalledWith(2, 'work', 'TASK-1', { scope: 'series', remove_deadline: true, reason: 'Not needed', revision: 'rev1', acknowledge_violations: true });
});
it('re-previews on commitment_changed and applies with the new revision', async () => {
 h.get.mockResolvedValue(native());
 h.reneg.mockResolvedValueOnce(previewResult('rev1')).mockRejectedValueOnce({ status: 409, code: 'commitment_changed', message: 'The commitment changed; re-preview before applying' })
  .mockResolvedValueOnce(previewResult('rev2')).mockResolvedValueOnce(native());
 const { onsaved } = mount(native());
 await fireEvent.click(screen.getByLabelText('Lapses'));
 await input('Reason for the deadline change', 'Softer'); await save();
 await fireEvent.click(await screen.findByRole('button', { name: 'Change deadline' }));
 expect(await screen.findByText('The date changed meanwhile. Review the new preview and confirm again.')).toBeTruthy();
 expect(onsaved).not.toHaveBeenCalled();
 await fireEvent.click(screen.getByRole('button', { name: 'Change deadline' }));
 await waitFor(() => expect(onsaved).toHaveBeenCalledTimes(1));
 expect(h.calls).toEqual(['get', 'preview', 'apply', 'preview', 'apply']);
 expect(h.reneg).toHaveBeenLastCalledWith('work', 'TASK-1', { scope: 'series', deadline_behavior: 'lapses', reason: 'Softer', revision: 'rev2' });
});
const lapseWarning = 'This occurrence lapses because its deadline passed. Confirming closes it as lapsed and does not apply this change.';
const lapsing = (revision: string) => { const r = previewResult(revision); r.preview.miss_recorded_first = true; r.preview.lapses_on_apply = true; return r; };
it('warns in the review when apply lapses the occurrence, and not otherwise', async () => {
 h.get.mockResolvedValue(native()); h.reneg.mockResolvedValueOnce(lapsing('rev1'));
 mount(native());
 await fireEvent.click(screen.getByLabelText('Lapses'));
 await input('Reason for the deadline change', 'Softer'); await save();
 const panel = await screen.findByRole('region', { name: 'Review the deadline change' });
 const warning = [...panel.querySelectorAll('p')].find(p => p.textContent === lapseWarning);
 expect(warning?.classList.contains('warn')).toBe(true);
 cleanup();
 h.reneg.mockResolvedValueOnce(previewResult('rev1'));
 mount(native());
 await fireEvent.click(screen.getByLabelText('Lapses'));
 await input('Reason for the deadline change', 'Softer'); await save();
 const quiet = await screen.findByRole('region', { name: 'Review the deadline change' });
 expect(quiet.textContent).not.toContain('lapses because');
});
it('explains the conflict as the lapse when the confirmed preview promised one', async () => {
 h.get.mockResolvedValue(native());
 h.reneg.mockResolvedValueOnce(lapsing('rev1')).mockRejectedValueOnce({ status: 409, code: 'commitment_changed', message: 'The commitment changed; re-preview before applying' })
  .mockResolvedValueOnce(previewResult('rev2'));
 const { onsaved } = mount(native());
 await fireEvent.click(screen.getByLabelText('Lapses'));
 await input('Reason for the deadline change', 'Softer'); await save();
 await fireEvent.click(await screen.findByRole('button', { name: 'Change deadline' }));
 expect(await screen.findByText('The deadline had passed, so the occurrence lapsed. Review the new preview and confirm again.')).toBeTruthy();
 expect(screen.queryByText(/The date changed meanwhile/)).toBeNull();
 expect(screen.queryByText(lapseWarning)).toBeNull();
 expect(onsaved).not.toHaveBeenCalled();
 expect(h.calls).toEqual(['get', 'preview', 'apply', 'preview']);
});
it('shows the server sentence for deadline_owner_required on preview', async () => {
 h.get.mockResolvedValue(native());
 h.reneg.mockRejectedValueOnce({ status: 403, code: 'deadline_owner_required', message: 'A schedule with a hard deadline is removed or changed only by the item\'s assignee or a workspace owner' });
 mount(native());
 await fireEvent.click(screen.getByLabelText('Lapses'));
 await input('Reason for the deadline change', 'x'); await save();
 expect(await screen.findByText(/only by the item's assignee or a workspace owner/)).toBeTruthy();
 expect(h.calls).toEqual(['get', 'preview']);
});
it.each([
 [{ status: 409, code: 'schedule_linked', message: 'A linked item you cannot see depends on this date.' }, 'A linked item you cannot see depends on this date.'],
 [{ status: 403, code: 'forbidden', message: 'nope' }, 'You can no longer change this item.'],
 [{ status: 400, code: 'validation_error', message: 'rrule is invalid', details: { field: 'rrule', code: 'format' } }, 'rrule is invalid'],
 [{ status: 503, code: 'database_busy', message: '' }, 'Could not update the date. Try again.']
])('keeps the input on a refused PATCH %j', async (error, text) => {
 h.get.mockResolvedValue(plain()); h.update.mockRejectedValueOnce(error);
 const { onsaved } = mount(plain());
 await fireEvent.change(screen.getByLabelText('Repeats'), { target: { value: 'daily' } }); await save();
 expect(await screen.findByText(text)).toBeTruthy();
 expect(h.update).toHaveBeenCalledWith('work', 'TASK-1', { rrule: 'FREQ=DAILY' }); expect(onsaved).not.toHaveBeenCalled();
});
it('discards the answer after the account identity changes', async () => {
 const pending = deferred<ItemCommitmentView>(); h.get.mockReturnValueOnce(pending.promise);
 const { onsaved } = mount(plain());
 await input('Target date', '2031-03-17'); await save();
 h.auth.identityEpoch++; pending.resolve(plain()); await Promise.resolve(); await Promise.resolve();
 expect(h.calls).toEqual(['get']); expect(onsaved).not.toHaveBeenCalled();
});
it('keeps a one-off deadline at its offset from the target as the target moves', async () => {
 const once = { ...plain(), rrule: undefined };
 h.get.mockResolvedValue(once);
 mount(once);
 await fireEvent.click(screen.getByLabelText('Hard deadline'));
 await input('Deadline date', '2031-03-12'); await input('Deadline time', '17:00');
 await input('Target date', '2031-03-11');
 expect((screen.getByLabelText('Deadline date') as HTMLInputElement).value).toBe('2031-03-13');
 await save();
 await waitFor(() => expect(h.update).toHaveBeenCalledWith('work', 'TASK-1', { dtstart: '2031-03-11T09:00:00', roles: { deadline_days: 2, deadline_secs: 28800, deadline_behavior: 'lapses' } }));
});
it('ignores a deadline edit while the target date is empty, keeping the offset', async () => {
 const once = { ...plain(), rrule: undefined };
 h.get.mockResolvedValue(once);
 mount(once);
 await fireEvent.click(screen.getByLabelText('Hard deadline'));
 await input('Deadline date', '2031-03-12'); await input('Deadline time', '17:00');
 await input('Target date', '');
 await input('Deadline time', '18:00');
 await input('Target date', '2031-03-10');
 await save();
 await waitFor(() => expect(h.update).toHaveBeenCalledWith('work', 'TASK-1', { roles: { deadline_days: 2, deadline_secs: 28800, deadline_behavior: 'lapses' } }));
});
it('formats the review rows: Applies from as a date, a spaced separator before the behavior', async () => {
 h.get.mockResolvedValue(native()); h.reneg.mockResolvedValueOnce(previewResult('rev1'));
 mount(native());
 await fireEvent.click(screen.getByLabelText('Lapses'));
 await input('Reason for the deadline change', 'Softer'); await save();
 const panel = await screen.findByRole('region', { name: 'Review the deadline change' });
 const rows = [...panel.querySelectorAll('p')].map(p => p.textContent ?? '');
 const row = (k: string) => rows.find(r => r.startsWith(k)) ?? '';
 expect(row('Deadline now')).toBe(`Deadline now ${roleText('2031-03-11T05:00:00Z', 'America/New_York')} · Still owed`);
 expect(row('New deadline')).toBe(`New deadline ${roleText('2031-03-12T05:00:00Z', 'America/New_York')} · Still owed`);
 expect(row('Applies from')).toBe(`Applies from ${roleText('2031-03-10T13:00:00Z', 'America/New_York')}`);
 expect(panel.textContent).not.toContain('|');
});
it('leaves Applies from out when the preview has none', async () => {
 h.get.mockResolvedValue(native()); const r = previewResult('rev1'); r.preview.applies_from = ''; h.reneg.mockResolvedValueOnce(r);
 mount(native());
 await fireEvent.click(screen.getByLabelText('Lapses'));
 await input('Reason for the deadline change', 'Softer'); await save();
 const panel = await screen.findByRole('region', { name: 'Review the deadline change' });
 expect(panel.textContent).not.toContain('Applies from');
});
it('keeps the deadline offset when Deadline time is cleared on a timed target', async () => {
 mount(native());
 await input('Deadline time', '');
 await save();
 expect(await screen.findByText('No changes.')).toBeTruthy();
 expect(h.calls).toEqual([]);
});
it('lays each labelled review row out as a label cell and a value cell', async () => {
 h.get.mockResolvedValue(native()); h.reneg.mockResolvedValueOnce(previewResult('rev1'));
 mount(native());
 await fireEvent.click(screen.getByLabelText('Lapses'));
 await input('Reason for the deadline change', 'Softer'); await save();
 const panel = await screen.findByRole('region', { name: 'Review the deadline change' });
 const rows = [...panel.querySelectorAll('.k')].map(k => k.parentElement!);
 expect(rows.map(r => r.firstElementChild!.textContent)).toEqual(['Deadline now', 'New deadline', 'Applies from']);
 for (const r of rows) { expect(r.classList.contains('row')).toBe(true); expect(r.children).toHaveLength(2); }
 expect(rows[0].children[1].textContent).toBe(`${roleText('2031-03-11T05:00:00Z', 'America/New_York')} · Still owed`);
});
const staleRevision = { status: 409, code: 'commitment_changed', message: 'x', details: { field: 'expected_revision', code: 'stale' } };
it("sends the opened view's commitment_revision with the PATCH", async () => {
 const opened = { ...plain(), commitment_revision: 'r0' };
 h.get.mockResolvedValue(opened);
 const { onsaved } = mount(opened);
 await input('Target date', '2031-03-17'); await save();
 await waitFor(() => expect(onsaved).toHaveBeenCalledTimes(1));
 expect(h.update).toHaveBeenCalledWith('work', 'TASK-1', { dtstart: '2031-03-17T09:00:00', expected_revision: 'r0' });
});
it('a stale expected_revision on PATCH shows Reload and writes nothing more', async () => {
 const opened = { ...plain(), commitment_revision: 'r0' };
 h.get.mockResolvedValue(opened); h.update.mockRejectedValueOnce(staleRevision);
 const { onsaved, onreload } = mount(opened);
 await input('Target date', '2031-03-17'); await save();
 expect(await screen.findByText(/This date changed\. Reload to edit the current values\./)).toBeTruthy();
 expect(onsaved).not.toHaveBeenCalled();
 expect((screen.getByRole('button', { name: 'Save' }) as HTMLButtonElement).disabled).toBe(true);
 await fireEvent.click(screen.getByRole('button', { name: 'Reload' })); expect(onreload).toHaveBeenCalledTimes(1);
 expect(h.calls).toEqual(['get', 'patch']);
});
it('sends expected_revision on preview only, not apply', async () => {
 const opened = { ...native(), commitment_revision: 'r0' };
 h.get.mockResolvedValue(opened); h.reneg.mockResolvedValueOnce(previewResult('rev1')).mockResolvedValueOnce(opened);
 const { onsaved } = mount(opened);
 await input('Days after the target', '2');
 await input('Reason for the deadline change', 'Client asked'); await save();
 await fireEvent.click(await screen.findByRole('button', { name: 'Change deadline' }));
 await waitFor(() => expect(onsaved).toHaveBeenCalledTimes(1));
 expect(h.reneg).toHaveBeenNthCalledWith(1, 'work', 'TASK-1', { scope: 'series', deadline_offset: { deadline_days: 2, deadline_secs: -28800 }, reason: 'Client asked', preview: true, expected_revision: 'r0' });
 expect(h.reneg).toHaveBeenNthCalledWith(2, 'work', 'TASK-1', { scope: 'series', deadline_offset: { deadline_days: 2, deadline_secs: -28800 }, reason: 'Client asked', revision: 'rev1' });
});
it('a stale preview shows Reload', async () => {
 const opened = { ...native(), commitment_revision: 'r0' };
 h.get.mockResolvedValue(opened); h.reneg.mockRejectedValueOnce(staleRevision);
 const { onsaved } = mount(opened);
 await fireEvent.click(screen.getByLabelText('Lapses'));
 await input('Reason for the deadline change', 'Softer'); await save();
 expect(await screen.findByText(/This date changed\. Reload to edit the current values\./)).toBeTruthy();
 expect(screen.getByRole('button', { name: 'Reload' })).toBeTruthy();
 expect(onsaved).not.toHaveBeenCalled();
 expect(h.calls).toEqual(['get', 'preview']);
});
