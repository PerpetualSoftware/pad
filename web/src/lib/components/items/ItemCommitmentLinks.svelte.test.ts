import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
const h = vi.hoisted(() => ({ get: vi.fn(), gates: vi.fn(), auth: { identityEpoch: 0, user: { timezone: 'UTC' } } }));
vi.mock('$lib/api/client', () => ({ api: { items: { get: h.get }, getCommitmentGates: h.gates } }));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { get identityEpoch() { return h.auth.identityEpoch; }, get user() { return h.auth.user; }, identityFence() { const epoch = h.auth.identityEpoch; return () => epoch === h.auth.identityEpoch; } } }));
import ItemCommitmentLinks from './ItemCommitmentLinks.svelte';
import { gate, item, link, otherId, view } from '$lib/test/commitmentFixtures';
import type { ItemCommitmentSnapshot } from '$lib/commitment';
function props(v: ReturnType<typeof view> | null = view({ links: [link(), link({ id: 'second' })], gates: { state: 'waiting', gates: [gate()] } })) {
 const snapshot: ItemCommitmentSnapshot = { wsSlug: 'work', itemId: item.id, itemSlug: 'TASK-1', authEpoch: 0, zone: 'UTC', view: v, editable: false };
 return { wsSlug: 'work', username: 'dave', itemId: item.id, itemSlug: 'TASK-1', snapshot };
}
beforeEach(() => { vi.resetAllMocks(); h.get.mockResolvedValue({ ...item, id: otherId, item_number: 2, title: 'Prepare release' }); h.gates.mockResolvedValue({ state: 'released', gates: [] }); });
afterEach(cleanup);
it('names linked items, dedupes link/gate fetches and uses item navigation', async () => {
 const onOpenTarget = vi.fn(); const { container } = render(ItemCommitmentLinks, { ...props(), onOpenTarget });
 const names = await screen.findAllByRole('link', { name: 'TASK-2 · Prepare release' });
 expect(names).toHaveLength(3); expect(names[0]).toHaveAttribute('href', '/dave/work/tasks/TASK-2');
 expect(h.get).toHaveBeenCalledTimes(1); expect(h.get).toHaveBeenCalledWith('work', otherId);
 expect(container.textContent).not.toMatch(/[0-9a-f]{8}-[0-9a-f-]{27}/);
 await fireEvent.click(names[0]); expect(onOpenTarget).toHaveBeenCalledWith(expect.objectContaining({ ref: 'TASK-2', collectionSlug: 'tasks' }));
});
it('falls back to unlinked Linked item on failed hydration, never UUID text', async () => {
 h.get.mockRejectedValue(new Error(otherId)); const { container } = render(ItemCommitmentLinks, props());
 await waitFor(() => expect(h.get).toHaveBeenCalledTimes(1));
 await screen.findAllByText('Linked item'); expect(screen.queryByRole('link')).toBeNull();
 expect(container.textContent).not.toContain(otherId);
});
it('shows the incoming gates a no-schedule snapshot carries, without reading them again', async () => {
 const p = props(null); render(ItemCommitmentLinks, { ...p, snapshot: { ...p.snapshot, gates: { state: 'waiting', gates: [gate()] } } });
 await screen.findByRole('link', { name: 'TASK-2 · Prepare release' });
 expect(h.gates).not.toHaveBeenCalled(); expect(screen.getByText('Waiting')).toBeTruthy();
});
it('keeps an aggregate with hidden blockers and no hidden count', async () => {
 const { container } = render(ItemCommitmentLinks, props(view({ gates: { state: 'blocked_needs_decision', gates: [] } })));
 await screen.findByText('Blocked — decision needed'); expect(h.get).not.toHaveBeenCalled(); expect(container.textContent).not.toMatch(/hidden|\d/);
});
it('renders nothing and reads nothing for a no-schedule snapshot without gates', async () => {
 const { container } = render(ItemCommitmentLinks, props(null));
 await Promise.resolve(); expect(container.textContent).toBe(''); expect(h.gates).not.toHaveBeenCalled(); expect(h.get).not.toHaveBeenCalled();
});
it('renders nothing for an empty snapshot', async () => {
 const { container } = render(ItemCommitmentLinks, props(view()));
 await Promise.resolve(); expect(container.textContent).toBe(''); expect(h.get).not.toHaveBeenCalled(); expect(h.gates).not.toHaveBeenCalled();
});
it('discards stale item hydration when the snapshot switches', async () => {
 let resolve!: (v: typeof item) => void;
 h.get.mockReturnValueOnce(new Promise<typeof item>(r => { resolve = r; }));
 const { rerender, container } = render(ItemCommitmentLinks, props());
 await waitFor(() => expect(h.get).toHaveBeenCalledTimes(1));
 await rerender(props(view())); resolve({ ...item, id: otherId, title: 'Old secret title' });
 await Promise.resolve(); expect(container.textContent).toBe('');
});
it('keeps rows and reuses names when the same item refreshes', async () => {
 const p = props(); const { rerender } = render(ItemCommitmentLinks, p);
 await screen.findAllByRole('link', { name: 'TASK-2 · Prepare release' });
 await rerender({ ...p, snapshot: { ...p.snapshot } });
 expect(screen.getAllByRole('link', { name: 'TASK-2 · Prepare release' }).length).toBeGreaterThan(0);
 await waitFor(() => expect(screen.getAllByRole('link', { name: 'TASK-2 · Prepare release' })).toHaveLength(3));
 expect(h.get).toHaveBeenCalledTimes(1);
});
