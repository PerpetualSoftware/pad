import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { flushSync } from 'svelte';
const h = vi.hoisted(() => ({ get: vi.fn(), gates: vi.fn(), resolve: vi.fn(), create: vi.fn(), subscribe: vi.fn(), auth: { identityEpoch: 0, user: { timezone: 'UTC' } } }));
vi.mock('$lib/api/client', () => ({ api: { getCommitment: h.get, getCommitmentGates: h.gates, resolveCommitment: h.resolve, createCommitment: h.create } }));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { get identityEpoch() { return h.auth.identityEpoch; }, get user() { return h.auth.user; }, identityFence() { const epoch = h.auth.identityEpoch; return () => epoch === h.auth.identityEpoch; } } }));
vi.mock('$lib/services/sse.svelte', () => ({ sseService: { onItemEvent: h.subscribe } }));
import ItemCommitmentPanel from './ItemCommitmentPanel.svelte';
import { collection, item, view } from '$lib/test/commitmentFixtures';
import type { ItemCommitmentView } from '$lib/types';
const props = () => ({ wsSlug: 'work', itemSlug: 'TASK-1', item, collection, onSnapshot: vi.fn() });
beforeEach(() => { vi.resetAllMocks(); h.auth.identityEpoch = 0; h.get.mockResolvedValue(view()); h.gates.mockResolvedValue({ state: 'released', gates: [] }); h.subscribe.mockReturnValue(vi.fn()); vi.spyOn(Date, 'now').mockReturnValue(Date.parse('2031-03-10T12:00:00Z')); });
afterEach(() => { cleanup(); vi.restoreAllMocks(); });
it('renders roles, recurrence, forecast and state and publishes an identity-tagged snapshot', async () => {
 const p = props(); render(ItemCommitmentPanel, p);
 await screen.findByRole('heading', { name: 'When' });
 for (const text of ['Target', 'March 10, 2031', 'every Monday', 'Deadline', 'Still owed', 'Earliest', 'Occupied end', 'Forecast March 12, 2031 · set by Alex', 'Outstanding']) expect(screen.getByText(text)).toBeTruthy();
 expect(screen.getByRole('img', { name: 'Outstanding' })).toBeTruthy();
 expect(p.onSnapshot).toHaveBeenLastCalledWith(expect.objectContaining({ wsSlug: 'work', itemId: item.id, itemSlug: 'TASK-1', authEpoch: 0, view: view() }));
});
it('renders nothing for not_found (404)', async () => {
 h.get.mockRejectedValue({ code: 'not_found' }); const p = props(); const { container } = render(ItemCommitmentPanel, p);
 await waitFor(() => expect(p.onSnapshot).toHaveBeenLastCalledWith(expect.objectContaining({ view: null })));
 expect(container.textContent).toBe('');
});
it('shows unavailable for a server error and retries only the read', async () => {
 h.get.mockRejectedValueOnce({ code: 'internal_error' }); render(ItemCommitmentPanel, props());
 await screen.findByText('Date unavailable'); await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
 await screen.findByText('every Monday'); expect(h.get).toHaveBeenCalledTimes(2);
});
it('loads once per item, including after status/title rerenders', async () => {
 const p = props(); const { rerender } = render(ItemCommitmentPanel, p); await screen.findByText('every Monday');
 await rerender({ ...p, item: { ...item, title: 'New title', fields: '{"status":"shipped"}' } });
 await screen.findByText('Outstanding · Item closed'); expect(h.get).toHaveBeenCalledTimes(1);
});
it('discards a late response after switching items', async () => {
 let resolve!: (value: ItemCommitmentView) => void;
 h.get.mockReturnValueOnce(new Promise<ItemCommitmentView>(r => { resolve = r; }));
 const p = props(); const { rerender } = render(ItemCommitmentPanel, p);
 await waitFor(() => expect(h.get).toHaveBeenCalledTimes(1));
 const next = { ...item, id: 'new-item', item_number: 2 };
 h.get.mockResolvedValue(view({ item_id: next.id, rrule: 'FREQ=DAILY' }));
 await rerender({ ...p, item: next, itemSlug: 'TASK-2' }); await screen.findByText('every day');
 resolve(view()); await Promise.resolve(); flushSync();
 expect(screen.queryByText('every Monday')).toBeNull();
 expect(p.onSnapshot).toHaveBeenLastCalledWith(expect.objectContaining({ itemId: next.id, view: expect.objectContaining({ rrule: 'FREQ=DAILY' }) }));
});
it('does not publish after account identity changes or disposal', async () => {
 let resolve!: (value: ItemCommitmentView) => void;
 h.get.mockReturnValueOnce(new Promise<ItemCommitmentView>(r => { resolve = r; }));
 const p = props(); const { unmount } = render(ItemCommitmentPanel, p);
 await waitFor(() => expect(h.get).toHaveBeenCalledTimes(1)); h.auth.identityEpoch++;
 resolve(view()); await Promise.resolve(); flushSync();
 expect(p.onSnapshot.mock.calls.some(([s]) => s.view)).toBe(false);
 await unmount();
});
it('refreshes for matching item_updated events only and coalesces a burst', async () => {
 render(ItemCommitmentPanel, props()); await screen.findByText('every Monday');
 const event = h.subscribe.mock.calls[0][0];
 event({ type: 'item_updated', item_id: 'other' });
 event({ type: 'item_updated', item_id: item.id }); event({ type: 'item_updated', item_id: item.id });
 await waitFor(() => expect(h.get).toHaveBeenCalledTimes(2));
});
it('keeps specific dates expandable and date-only values out of timezone conversion', async () => {
 h.get.mockResolvedValue(view({ rrule: undefined, rdates: ['2031-03-12'], exdates: ['2031-03-13'] }));
 render(ItemCommitmentPanel, props()); await screen.findByText('specific dates');
 expect(screen.getByText('Additional dates').closest('details')).toHaveTextContent('2031-03-12');
 expect(screen.getByText('Excluded dates').closest('details')).toHaveTextContent('2031-03-13');
});
it('keeps the shown view while an item_updated refresh is in flight', async () => {
 const p = props(); render(ItemCommitmentPanel, p); await screen.findByText('every Monday');
 const published = p.onSnapshot.mock.calls.length;
 let resolve!: (value: ItemCommitmentView) => void;
 h.get.mockReturnValueOnce(new Promise<ItemCommitmentView>(r => { resolve = r; }));
 h.subscribe.mock.calls[0][0]({ type: 'item_updated', item_id: item.id });
 await waitFor(() => expect(h.get).toHaveBeenCalledTimes(2));
 flushSync();
 expect(screen.getByText('every Monday')).toBeTruthy();
 expect(p.onSnapshot.mock.calls.slice(published).some(([s]) => s.view === undefined)).toBe(false);
 resolve(view({ rrule: 'FREQ=DAILY' })); await screen.findByText('every day');
});
it('keeps the view but offers Retry when a background refresh fails', async () => {
 render(ItemCommitmentPanel, props()); await screen.findByText('every Monday');
 h.get.mockRejectedValueOnce({ code: 'internal_error' });
 h.subscribe.mock.calls[0][0]({ type: 'item_updated', item_id: item.id });
 await screen.findByText(/Couldn't refresh these dates/);
 expect(screen.getByText('every Monday')).toBeTruthy();
 h.get.mockResolvedValueOnce(view({ rrule: 'FREQ=DAILY' }));
 await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
 await screen.findByText('every day'); expect(screen.queryByText(/Couldn't refresh/)).toBeNull();
});
it('offers Add date to an editor when there is no commitment, from the gates answer', async () => {
 h.get.mockRejectedValue({ code: 'not_found' }); h.gates.mockResolvedValue({ state: 'released', gates: [], editable: true });
 const p = props(); render(ItemCommitmentPanel, p);
 await screen.findByRole('button', { name: '+ Add date' }); expect(screen.getByText('No date')).toBeTruthy();
 expect(h.gates).toHaveBeenCalledTimes(1); expect(h.gates).toHaveBeenCalledWith('work', 'TASK-1');
 expect(p.onSnapshot).toHaveBeenLastCalledWith(expect.objectContaining({ view: null, editable: true, gates: { state: 'released', gates: [], editable: true } }));
});
it.each([[false], [undefined]])('shows readers nothing without a commitment (editable %s)', async editable => {
 h.get.mockRejectedValue({ code: 'not_found' }); h.gates.mockResolvedValue({ state: 'waiting', gates: [], ...(editable === undefined ? {} : { editable }) });
 const p = props(); const { container } = render(ItemCommitmentPanel, p);
 await waitFor(() => expect(p.onSnapshot).toHaveBeenLastCalledWith(expect.objectContaining({ view: null, editable: false })));
 expect(container.textContent).toBe('');
});
it('shows unavailable, not Add date, when the no-schedule gates read fails', async () => {
 h.get.mockRejectedValue({ code: 'not_found' }); h.gates.mockRejectedValueOnce({ code: 'forbidden' });
 const p = props(); render(ItemCommitmentPanel, p);
 await screen.findByText('Date unavailable'); expect(screen.queryByRole('button', { name: '+ Add date' })).toBeNull();
 expect(p.onSnapshot.mock.calls.some(([s]) => s.view === null)).toBe(false);
 h.gates.mockResolvedValueOnce({ state: 'released', gates: [], editable: true });
 await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
 await screen.findByRole('button', { name: '+ Add date' });
});
it('adds a date, re-reads and announces it once', async () => {
 h.get.mockRejectedValueOnce({ code: 'not_found' }); h.gates.mockResolvedValue({ state: 'released', gates: [], editable: true }); h.create.mockResolvedValue(view());
 h.get.mockResolvedValue(view({ editable: true }));
 render(ItemCommitmentPanel, props());
 await fireEvent.click(await screen.findByRole('button', { name: '+ Add date' }));
 await fireEvent.input(screen.getByLabelText('Target date'), { target: { value: '2031-03-10' } });
 await fireEvent.click(screen.getByRole('button', { name: 'Add date' }));
 await screen.findByText('every Monday');
 expect(screen.getAllByText('Date added.')).toHaveLength(1);
 expect(screen.queryByRole('dialog')).toBeNull();
});
it('shows occurrence actions to an editor and re-reads only the commitment after one', async () => {
 h.get.mockResolvedValue(view({ editable: true })); h.resolve.mockResolvedValue({ commitment: view(), resolution: { occurrence_at: '', state: 'done', missed_recorded: 0 } });
 render(ItemCommitmentPanel, props());
 await fireEvent.click(await screen.findByRole('button', { name: 'Done TASK-1' }));
 await waitFor(() => expect(h.get).toHaveBeenCalledTimes(3));
 expect(h.resolve).toHaveBeenCalledTimes(1); expect(h.gates).not.toHaveBeenCalled();
 await screen.findByText('Occurrence marked done.');
});
it.each([[{}], [{ editable: false }]])('shows a reader no buttons and no dialogs (%j)', async extra => {
 h.get.mockResolvedValue(view(extra)); render(ItemCommitmentPanel, props());
 await screen.findByText('every Monday');
 expect(screen.queryByRole('button')).toBeNull(); expect(screen.queryByRole('dialog')).toBeNull();
 expect(document.querySelector('[data-agenda-actions]')).toBeNull();
});
it.each([
 ['a closed item', {}, { fields: '{"status":"shipped"}' }],
 ['an ended schedule', { ended_at: '2031-03-09', next_occurrence: undefined }, {}],
 ['a schedule error', { error_code: 'bad_rule' }, {}]
])('shows no occurrence actions for %s', async (_name, extra, itemExtra) => {
 h.get.mockResolvedValue(view({ editable: true, ...extra })); render(ItemCommitmentPanel, { ...props(), item: { ...item, ...itemExtra } });
 await screen.findByRole('heading', { name: 'When' });
 expect(screen.queryByRole('button', { name: 'Done TASK-1' })).toBeNull();
});
