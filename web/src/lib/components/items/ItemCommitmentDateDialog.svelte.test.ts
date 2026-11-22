import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
const h = vi.hoisted(() => ({ create: vi.fn(), auth: { identityEpoch: 0, user: { timezone: 'America/New_York' } as { timezone?: string } | null } }));
vi.mock('$lib/api/client', () => ({ api: { createCommitment: h.create } }));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { get identityEpoch() { return h.auth.identityEpoch; }, get user() { return h.auth.user; }, identityFence() { const epoch = h.auth.identityEpoch; return () => epoch === h.auth.identityEpoch; } } }));
import ItemCommitmentDateDialog from './ItemCommitmentDateDialog.svelte';

function deferred<T>() { let resolve!: (v: T) => void; let reject!: (e: unknown) => void; const promise = new Promise<T>((a, b) => { resolve = a; reject = b; }); return { promise, resolve, reject }; }
const mount = () => { const oncreated = vi.fn(), onclose = vi.fn(); render(ItemCommitmentDateDialog, { open: true, wsSlug: 'work', itemSlug: 'TASK-1', itemTitle: 'Work', mobile: false, onclose, oncreated }); return { oncreated, onclose }; };
const input = (label: string, value: string) => fireEvent.input(screen.getByLabelText(label), { target: { value } });
const submit = () => fireEvent.click(screen.getByRole('button', { name: 'Add date' }));
beforeEach(() => { vi.resetAllMocks(); h.auth.identityEpoch = 0; h.auth.user = { timezone: 'America/New_York' }; h.create.mockResolvedValue({}); vi.spyOn(Date, 'now').mockReturnValue(Date.parse('2031-03-01T12:00:00Z')); });
afterEach(() => { cleanup(); vi.restoreAllMocks(); });

it('names the item by its issue ID, never its URL slug', async () => {
 render(ItemCommitmentDateDialog, { open: true, wsSlug: 'work', itemSlug: 'draft-the-guide', itemRef: 'BACK-11', itemTitle: 'Draft the guide', mobile: false, onclose: vi.fn(), oncreated: vi.fn() });
 await screen.findByText('BACK-11');
 expect(screen.queryByText('draft-the-guide')).toBeNull();
});
it('posts a date-only once commitment and reports success', async () => {
 const { oncreated } = mount();
 await input('Target date', '2031-03-10'); await submit();
 await waitFor(() => expect(h.create).toHaveBeenCalledWith('work', 'TASK-1', { dtstart: '2031-03-10', roles: { target_days: 0, target_secs: 0 } }));
 await waitFor(() => expect(oncreated).toHaveBeenCalledTimes(1));
});
it('posts a timed weekly commitment with a civil deadline across the DST change', async () => {
 mount();
 await input('Target date', '2031-03-08'); await input('Time (optional)', '09:00');
 await fireEvent.change(screen.getByLabelText('Repeats'), { target: { value: 'weekly' } });
 await fireEvent.click(screen.getByLabelText('Hard deadline'));
 await input('Deadline date', '2031-03-09'); await input('Deadline time', '09:00');
 await fireEvent.click(screen.getByLabelText('Still owed'));
 await submit();
 await waitFor(() => expect(h.create).toHaveBeenCalledWith('work', 'TASK-1', { dtstart: '2031-03-08T09:00', rrule: 'FREQ=WEEKLY', roles: { target_days: 0, target_secs: 0, deadline_days: 1, deadline_secs: 0, deadline_behavior: 'stays_owed' } }));
});
it('labels the recurrence choices from the chosen date', async () => {
 mount(); await input('Target date', '2031-03-10');
 const options = [...(screen.getByLabelText('Repeats') as HTMLSelectElement).options].map(o => o.textContent);
 expect(options).toEqual(['Once', 'every day', 'every weekday', 'every Monday', 'every other Monday', 'every month', 'last day of every month', 'every 3 months', 'every year']);
});
it('keeps one precision: no deadline time for a date-only target', async () => {
 mount(); await input('Target date', '2031-03-10');
 await fireEvent.click(screen.getByLabelText('Hard deadline'));
 expect(screen.getByLabelText('Deadline date')).toBeTruthy();
 expect(screen.queryByLabelText('Deadline time')).toBeNull();
 await input('Time (optional)', '09:00');
 expect(screen.getByLabelText('Deadline time')).toBeTruthy();
});
it('refuses a deadline before the target without posting', async () => {
 mount(); await input('Target date', '2031-03-10');
 await fireEvent.click(screen.getByLabelText('Hard deadline')); await input('Deadline date', '2031-03-09');
 await submit();
 expect(await screen.findByText('The deadline cannot be before the target.')).toBeTruthy();
 expect(h.create).not.toHaveBeenCalled();
});
it.each([
 [{ status: 400, code: 'validation_error', message: 'dtstart is not a valid wall', details: { field: 'dtstart', code: 'format' } }, 'dtstart is not a valid wall'],
 [{ status: 403, code: 'forbidden', message: 'nope' }, 'You can no longer change this item.'],
 [{ status: 503, code: 'database_busy', message: '' }, 'Could not add the date. Try again.']
])('keeps the input on %j', async (error, text) => {
 const { oncreated } = mount();
 h.create.mockRejectedValueOnce(error);
 await input('Target date', '2031-03-10'); await submit();
 expect(await screen.findByText(text)).toBeTruthy();
 expect((screen.getByLabelText('Target date') as HTMLInputElement).value).toBe('2031-03-10');
 expect(oncreated).not.toHaveBeenCalled();
});
it('treats schedule_exists as created and never retries the POST', async () => {
 const { oncreated } = mount();
 h.create.mockRejectedValueOnce({ status: 409, code: 'schedule_exists', message: 'exists' });
 await input('Target date', '2031-03-10'); await submit();
 await waitFor(() => expect(oncreated).toHaveBeenCalledTimes(1));
 expect(h.create).toHaveBeenCalledTimes(1);
});
it('sends one POST for a double click', async () => {
 const pending = deferred<unknown>(); h.create.mockReturnValueOnce(pending.promise);
 const { oncreated } = mount(); await input('Target date', '2031-03-10');
 await submit(); await submit();
 expect(h.create).toHaveBeenCalledTimes(1);
 pending.resolve({}); await waitFor(() => expect(oncreated).toHaveBeenCalledTimes(1));
});
it('drops the answer after the account identity changes', async () => {
 const pending = deferred<unknown>(); h.create.mockReturnValueOnce(pending.promise);
 const { oncreated } = mount(); await input('Target date', '2031-03-10'); await submit();
 h.auth.identityEpoch++; pending.resolve({}); await Promise.resolve(); await Promise.resolve();
 expect(oncreated).not.toHaveBeenCalled();
});
it('shows the account zone the server pins, else UTC with a note', async () => {
 mount(); expect(screen.getByText('Time zone: America/New_York')).toBeTruthy();
 cleanup(); h.auth.user = { }; mount();
 expect(screen.getByText('Time zone: UTC (no account time zone set)')).toBeTruthy();
});
// TASK-4699: "Start of my day" fills the time field with the signed-in user's
// day start; the request carries the wall it filled, never day_start.
it.each([
 ['07:30', '07:30'],
 [null, '09:00'],
 [undefined, '09:00']
])('Start of my day fills the time with day_start %s', async (dayStart, want) => {
 h.auth.user = { timezone: 'America/New_York', day_start: dayStart } as { timezone?: string };
 mount();
 await input('Target date', '2031-03-10');
 await fireEvent.click(screen.getByRole('button', { name: 'Start of my day' }));
 expect((screen.getByLabelText('Time (optional)') as HTMLInputElement).value).toBe(want);
 await submit();
 await waitFor(() => expect(h.create).toHaveBeenCalledWith('work', 'TASK-1', { dtstart: `2031-03-10T${want}`, roles: { target_days: 0, target_secs: 0 } }));
 expect(h.create.mock.calls[0][2]).not.toHaveProperty('day_start');
});
