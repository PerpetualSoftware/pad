import { beforeEach, expect, it, vi } from 'vitest';

const h = vi.hoisted(() => ({ agents: vi.fn(), queue: vi.fn(), epoch: 0, reset: () => {} }));
vi.mock('$lib/api/client', () => ({ api: { getMyAgents: h.agents, agentRoles: { queue: h.queue } } }));
vi.mock('./auth.svelte', () => ({ authStore: {
	identityFence: () => { const epoch = h.epoch; return () => epoch === h.epoch; },
	onIdentityChange: (reset: () => void) => { h.reset = reset; return () => {}; }
} }));
import { agentsOverview } from './agentsOverview.svelte';

const lane = (queue: number) => ({ workspace: 'ws', role: { slug: 'impl', name: 'Implementer', icon: '' }, can_reorder: true, can_park: true, now: [], queue: Array.from({ length: queue }, (_, i) => ({ ref: `TASK-${i}`, title: 't', status: 'open', priority: '', role_sort_order: i * 1024, blocked: false, ready: true, can_edit: true })), queue_total: 9, parked_total: 0, queue_more: queue < 9 });
const answer = (name: string) => ({ agents: [{ slug: 'impl', name, icon: '', lanes: [lane(5)], finished: [], summary: { now_count: 0, queue_total: 9, parked: 'none' } }], degraded: [] });
function deferred<T>() { let resolve!: (v: T) => void; const promise = new Promise<T>(r => { resolve = r; }); return { promise, resolve }; }

beforeEach(() => { h.epoch++; h.reset(); vi.resetAllMocks(); h.agents.mockResolvedValue(answer('A')); });

it('an identity change resets the data and drops the in-flight result', async () => {
	await agentsOverview.load();
	expect(agentsOverview.data?.agents[0].name).toBe('A');
	agentsOverview.setOverlay('ws/impl', { refs: [], line: null });
	const read = deferred<ReturnType<typeof answer>>(); h.agents.mockReturnValueOnce(read.promise);
	const pending = agentsOverview.load();
	h.epoch++; h.reset();
	expect(agentsOverview.data).toBeNull();
	expect(agentsOverview.overlay).toEqual({});
	expect(agentsOverview.loading).toBe(false);
	read.resolve(answer('previous person')); await pending;
	expect(agentsOverview.data).toBeNull();
});

it('only the latest load publishes, and a published load clears the overlay', async () => {
	const older = deferred<ReturnType<typeof answer>>(); const newer = deferred<ReturnType<typeof answer>>();
	h.agents.mockReturnValueOnce(older.promise).mockReturnValueOnce(newer.promise);
	const first = agentsOverview.load(); const second = agentsOverview.load();
	agentsOverview.setOverlay('ws/impl', { refs: ['TASK-1'], line: null });
	newer.resolve(answer('new')); await second;
	expect(agentsOverview.data?.agents[0].name).toBe('new');
	expect(agentsOverview.overlay).toEqual({});
	older.resolve(answer('old')); await first;
	expect(agentsOverview.data?.agents[0].name).toBe('new');
});

it('Show all reads the lane queue at 50 and keeps it across refreshes', async () => {
	h.queue.mockResolvedValue(lane(9));
	await agentsOverview.expandLane('ws/impl');
	expect(h.queue).toHaveBeenCalledWith('ws', 'impl', 50);
	expect(agentsOverview.data?.agents[0].lanes[0].queue).toHaveLength(9);
	await agentsOverview.load();
	expect(h.queue).toHaveBeenCalledTimes(2);
});

it('a failed read shows an error without dropping what was loaded', async () => {
	await agentsOverview.load();
	h.agents.mockRejectedValueOnce(new Error('offline'));
	await agentsOverview.load();
	expect(agentsOverview.error).not.toBe('');
	expect(agentsOverview.data?.agents).toHaveLength(1);
});

it('a lane write stays pending until a load started after it publishes; a failure or identity change clears it', async () => {
	await agentsOverview.load();
	const older = deferred<ReturnType<typeof answer>>(); h.agents.mockReturnValueOnce(older.promise);
	const stale = agentsOverview.load();
	agentsOverview.beginWrite('ws/impl');
	expect(agentsOverview.isPending('ws/impl')).toBe(true);
	agentsOverview.endWrite('ws/impl', true);
	older.resolve(answer('before the write')); await stale;
	expect(agentsOverview.isPending('ws/impl')).toBe(true);
	await agentsOverview.load();
	expect(agentsOverview.isPending('ws/impl')).toBe(false);

	agentsOverview.beginWrite('ws/impl');
	await agentsOverview.load();
	expect(agentsOverview.isPending('ws/impl')).toBe(true);
	agentsOverview.endWrite('ws/impl', false);
	expect(agentsOverview.isPending('ws/impl')).toBe(false);

	agentsOverview.beginWrite('ws/impl');
	h.epoch++; h.reset();
	expect(agentsOverview.isPending('ws/impl')).toBe(false);
});
