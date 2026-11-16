import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { flushSync } from 'svelte';
const h = vi.hoisted(() => ({ get: vi.fn(), subscribe: vi.fn(), auth: { identityEpoch: 0, user: { timezone: 'UTC' } } }));
vi.mock('$lib/api/client', () => ({ api: { getCommitment: h.get } }));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { get identityEpoch() { return h.auth.identityEpoch; }, get user() { return h.auth.user; }, identityFence() { const epoch = h.auth.identityEpoch; return () => epoch === h.auth.identityEpoch; } } }));
vi.mock('$lib/services/sse.svelte', () => ({ sseService: { onItemEvent: h.subscribe } }));
import ItemCommitmentPanel from './ItemCommitmentPanel.svelte';
import { collection, item, view } from '$lib/test/commitmentFixtures';
import type { ItemCommitmentView } from '$lib/types';
const props = () => ({ wsSlug: 'work', itemSlug: 'TASK-1', item, collection, onSnapshot: vi.fn() });
beforeEach(() => { vi.resetAllMocks(); h.auth.identityEpoch = 0; h.get.mockResolvedValue(view()); h.subscribe.mockReturnValue(vi.fn()); vi.spyOn(Date, 'now').mockReturnValue(Date.parse('2031-03-10T12:00:00Z')); });
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
