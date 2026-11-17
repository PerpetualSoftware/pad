import { afterEach, beforeEach, expect, it, vi } from 'vitest';

const h = vi.hoisted(() => ({
 needs: vi.fn(), agenda: vi.fn(), reset: () => {},
 userId: 'user-a', zone: 'UTC', epoch: 0
}));
vi.mock('$lib/api/client', () => ({ api: { needs: { list: h.needs }, getMyAgenda: h.agenda } }));
vi.mock('./auth.svelte', () => ({ authStore: {
 get userId() { return h.userId; }, get user() { return { timezone: h.zone }; },
 identityFence: () => { const epoch = h.epoch; return () => epoch === h.epoch; },
 onIdentityChange: (reset: () => void) => { h.reset = reset; return () => {}; }
} }));
import { homeOverview } from './homeOverview.svelte';

function deferred() {
 let resolve!: (value: { entries: []; priority_list: [] }) => void;
 const promise = new Promise<{ entries: []; priority_list: [] }>(r => { resolve = r; });
 return { promise, resolve: () => resolve({ entries: [], priority_list: [] }) };
}
function expectReads(count: number) {
 expect(h.needs).toHaveBeenCalledTimes(count);
 expect(h.agenda).toHaveBeenCalledTimes(count);
}
beforeEach(() => {
 h.reset(); vi.resetAllMocks(); h.userId = 'user-a'; h.zone = 'UTC'; h.epoch = 0;
 vi.useFakeTimers({ toFake: ['Date'] }); vi.setSystemTime(new Date('2026-11-13T14:00:00Z'));
 h.needs.mockResolvedValue({ rows: [], total: 0, hidden: 0, workspaces: [] });
 h.agenda.mockResolvedValue({ entries: [], priority_list: [] });
});
afterEach(() => vi.useRealTimers());

it('returns the same pending promise on arrival and refetches after it settles', async () => {
 const read = deferred(); h.agenda.mockReturnValueOnce(read.promise);
 const pending = homeOverview.load('/');
 expect(homeOverview.arrive('/')).toBe(pending);
 await Promise.resolve(); expectReads(1);
 read.resolve(); await pending;
 await homeOverview.arrive('/'); expectReads(2);
});
it('force-refetches during a pending load, whose settlement cannot clear the latest marker', async () => {
 const first = deferred(); const next = deferred();
 h.agenda.mockReturnValueOnce(first.promise).mockReturnValueOnce(next.promise);
 const pending = homeOverview.load('/');
 const forced = homeOverview.load('/', true);
 expect(forced).not.toBe(pending);
 await Promise.resolve(); expectReads(2);
 first.resolve(); await pending;
 expect(homeOverview.arrive('/')).toBe(forced);
 next.resolve(); await forced;
 await homeOverview.arrive('/'); expectReads(3);
});
it.each(['path', 'user', 'zone', 'today'])('refetches when the %s key changes during a pending load', async key => {
 const read = deferred(); h.agenda.mockReturnValueOnce(read.promise);
 const pending = homeOverview.load('/');
 if (key === 'user') h.userId = 'user-b';
 if (key === 'zone') h.zone = 'Europe/Berlin';
 if (key === 'today') vi.setSystemTime(new Date('2026-11-14T14:00:00Z'));
 const arrival = homeOverview.arrive(key === 'path' ? '/needs' : '/');
 expect(arrival).not.toBe(pending);
 await arrival; expectReads(2);
 read.resolve(); await pending;
});
it('clears the in-flight marker on identity reset even for the same key', async () => {
 const read = deferred(); h.agenda.mockReturnValueOnce(read.promise);
 const pending = homeOverview.load('/');
 h.epoch++; h.reset();
 const arrival = homeOverview.arrive('/');
 expect(arrival).not.toBe(pending);
 await arrival; expectReads(2);
 read.resolve(); await pending;
});
it('refetches on arrival after a failed read has settled', async () => {
 h.agenda.mockRejectedValueOnce(new Error('offline'));
 await homeOverview.load('/');
 expect(homeOverview.error).not.toBe('');
 await homeOverview.arrive('/'); expectReads(2);
 expect(homeOverview.error).toBe('');
});
it('an older forced load that resolves last does not overwrite the newer one', async () => {
 const older = deferred(); const newer = deferred();
 h.agenda.mockReturnValueOnce(older.promise).mockReturnValueOnce(newer.promise);
 h.needs.mockResolvedValueOnce({ rows: [], total: 1, hidden: 0, workspaces: [] })
  .mockResolvedValueOnce({ rows: [], total: 2, hidden: 0, workspaces: [] });
 const first = homeOverview.load('/', true);
 const second = homeOverview.load('/', true);
 await Promise.resolve(); expectReads(2);
 newer.resolve(); await second;
 expect(homeOverview.needs?.total).toBe(2);
 expect(homeOverview.loading).toBe(false);
 older.resolve(); await first;
 expect(homeOverview.needs?.total).toBe(2);
 expect(homeOverview.loading).toBe(false);
});
it('an older forced load that resolves first leaves loading set until the newer one lands', async () => {
 const older = deferred(); const newer = deferred();
 h.agenda.mockReturnValueOnce(older.promise).mockReturnValueOnce(newer.promise);
 h.needs.mockResolvedValueOnce({ rows: [], total: 1, hidden: 0, workspaces: [] })
  .mockResolvedValueOnce({ rows: [], total: 2, hidden: 0, workspaces: [] });
 const first = homeOverview.load('/', true);
 const second = homeOverview.load('/', true);
 older.resolve(); await first;
 expect(homeOverview.loading).toBe(true);
 newer.resolve(); await second;
 expect(homeOverview.needs?.total).toBe(2);
 expect(homeOverview.loading).toBe(false);
});
