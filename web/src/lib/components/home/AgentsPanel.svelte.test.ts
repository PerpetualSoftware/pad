import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte';
import { tick } from 'svelte';
import type { MeAgentsResponse, RoleQueueResponse } from '$lib/types';

// DOC-4851 section 4: the Home Agents panel.
const h = vi.hoisted(() => ({
	agents: vi.fn(), roleGet: vi.fn(), park: vi.fn(), unpark: vi.fn(), reorder: vi.fn(), rolesList: vi.fn(), queue: vi.fn(),
	itemGet: vi.fn(), itemUpdate: vi.fn(), toast: vi.fn(),
	mobile: false, epoch: 0, resets: new Set<() => void>(),
	live: [] as Array<() => unknown>
}));
vi.mock('$lib/api/client', () => ({ api: {
	getMyAgents: h.agents,
	getMyAgenda: vi.fn(),
	needs: { list: vi.fn() },
	agentRoles: { get: h.roleGet, park: h.park, unpark: h.unpark, reorder: h.reorder, list: h.rolesList, queue: h.queue },
	items: { get: h.itemGet, update: h.itemUpdate }
} }));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: {
	userId: 'u', user: { name: 'Mara', timezone: 'UTC' },
	identityFence: () => { const epoch = h.epoch; return () => epoch === h.epoch; },
	onIdentityChange: (reset: () => void) => { h.resets.add(reset); return () => h.resets.delete(reset); }
} }));
vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: { workspaces: [{ id: '1', slug: 'pad', name: 'Pad' }, { id: '2', slug: 'cloud', name: 'Cloud' }] } }));
vi.mock('$lib/stores/breakpoint.svelte', () => ({ MOBILE_MEDIA_QUERY: '(max-width: 768px)', viewport: { get isMobile() { return h.mobile; } } }));
vi.mock('$lib/stores/toast.svelte', () => ({ toastStore: { show: h.toast } }));
vi.mock('$lib/services/homeLive', () => ({ homeLive: { register: (cb: () => unknown) => { h.live.push(cb); return () => { h.live = h.live.filter(c => c !== cb); }; } } }));

import AgentsPanel from './AgentsPanel.svelte';

const item = (ref: string, sort: number, can_edit = true) => ({ ref, title: `Title ${ref}`, status: 'open', priority: 'high', role_sort_order: sort, blocked: false, ready: true, can_edit });
function lane(over: Partial<RoleQueueResponse> = {}): RoleQueueResponse {
	return {
		workspace: 'pad', role: { slug: 'impl', name: 'Implementer', icon: '🛠' }, can_reorder: true, can_park: true,
		now: [{ ref: 'TASK-9', title: 'Export check-only mode', status: 'in-progress', since: new Date(Date.now() - 2 * 3600_000).toISOString(), lease: { holder: 'w1', expires_at: '2030-01-01T00:00:00Z' }, live: 'busy' }],
		queue: [item('TASK-1', 1024), item('TASK-2', 2048), item('TASK-3', 3072)],
		park: { mode: 'line', index: 2, first_parked_ref: 'TASK-3', set_by: 'Dave', set_at: '2026-11-26T15:04:00Z', park_seq: 4 },
		queue_total: 3, parked_total: 1, queue_more: false, ...over
	};
}
function answer(lanes: RoleQueueResponse[]): MeAgentsResponse {
	return { agents: [{ slug: 'impl', name: 'Implementer', icon: '🛠', lanes, finished: [], summary: { now_count: lanes.reduce((n, l) => n + l.now.length, 0), queue_total: lanes.reduce((n, l) => n + l.queue_total, 0), parked: 'some' } }], degraded: [] };
}
async function settle() { for (let i = 0; i < 6; i++) { await Promise.resolve(); await tick(); } }
async function open() {
	render(AgentsPanel, { compact: h.mobile });
	await settle();
	await fireEvent.click(screen.getByRole('button', { name: /Implementer/, expanded: false }));
	await settle();
}

beforeEach(() => {
	h.epoch++; for (const r of [...h.resets]) r();
	vi.clearAllMocks(); h.mobile = false; h.live = [];
	h.agents.mockResolvedValue(answer([lane(), lane({ workspace: 'cloud', can_reorder: false, can_park: false, park: undefined, now: [], parked_total: 0 })]));
	h.roleGet.mockResolvedValue({ park: { mode: 'line', sort_order: 3072, set_at: null, set_by: 'Dave', park_seq: 4 } });
	h.park.mockResolvedValue({ park: {} }); h.unpark.mockResolvedValue({}); h.reorder.mockResolvedValue({});
	h.itemGet.mockImplementation(async (_ws: string, ref: string) => ({ id: `id-${ref}` }));
});
afterEach(() => cleanup());

it('renders no panel when the caller has no agents', async () => {
	h.agents.mockResolvedValue({ agents: [], degraded: [] });
	render(AgentsPanel);
	await settle();
	expect(screen.queryByRole('region', { name: 'Agents' })).not.toBeInTheDocument();
});

it('shows one skeleton strip while loading and an inline Retry on error', async () => {
	let fail!: (e: Error) => void;
	h.agents.mockReturnValueOnce(new Promise((_, reject) => { fail = reject; }));
	render(AgentsPanel);
	await tick();
	expect(screen.getAllByTestId('agents-skeleton')).toHaveLength(1);
	fail(new Error('offline')); await settle();
	await fireEvent.click(within(screen.getByRole('region', { name: 'Agents' })).getByRole('button', { name: 'Retry' }));
	await settle();
	expect(h.agents).toHaveBeenCalledTimes(2);
	expect(screen.getByRole('button', { name: /Implementer/ })).toBeInTheDocument();
});

it('the collapsed strip shows Now, age, live state, queued count and parked lanes', async () => {
	render(AgentsPanel);
	await settle();
	const strip = screen.getByRole('button', { name: /Implementer/, expanded: false });
	expect(strip).toHaveTextContent('Now TASK-9 Export check-only mode Pad');
	expect(strip).toHaveTextContent('2h');
	expect(strip).toHaveTextContent('6 queued');
	expect(strip).toHaveTextContent('parked 1/2');
	expect(within(strip).getByRole('img', { name: 'Live: busy' })).toBeInTheDocument();
});

it('dragging the line sends park before the item below it, with expected_park_seq', async () => {
	await open();
	const padLane = screen.getByRole('region', { name: 'Implementer in Pad' });
	const line = within(padLane).getByRole('separator');
	expect(line).toHaveAccessibleName('Stop here. Set by Dave 15:04');
	expect(line).toHaveAttribute('draggable', 'true');
	const target = within(padLane).getByText('Title TASK-2').closest('li')!;
	await fireEvent.dragStart(line);
	await fireEvent.dragOver(target);
	await fireEvent.drop(target);
	await settle();
	expect(h.park).toHaveBeenCalledWith('pad', 'impl', { before: 'TASK-2', expected_park_seq: 4 });
	expect(h.agents).toHaveBeenCalledTimes(2);
});

it('a 409 park_conflict on a line drag refetches', async () => {
	h.park.mockRejectedValueOnce(Object.assign(new Error('conflict'), { code: 'park_conflict' }));
	await open();
	const padLane = screen.getByRole('region', { name: 'Implementer in Pad' });
	const target = within(padLane).getByText('Title TASK-1').closest('li')!;
	await fireEvent.dragStart(within(padLane).getByRole('separator'));
	await fireEvent.drop(target);
	await settle();
	expect(h.toast).toHaveBeenCalledWith(expect.stringContaining('stop line moved'), 'error');
	expect(h.agents).toHaveBeenCalledTimes(2);
});

it('Do next writes the item below the first one, through the role-board reorder', async () => {
	await open();
	await fireEvent.click(screen.getByRole('button', { name: 'Actions for TASK-3' }));
	await fireEvent.click(await screen.findByRole('menuitem', { name: /Do next/ }));
	await settle();
	expect(h.reorder).toHaveBeenCalledWith('pad', [{ item_id: 'id-TASK-3', role_sort_order: 0 }]);
});

it('Do next never crosses a top line and says so', async () => {
	h.agents.mockResolvedValue(answer([lane({ park: { mode: 'top', index: 0, first_parked_ref: 'TASK-1', set_by: 'Dave', set_at: null, park_seq: 2 } })]));
	await open();
	const padLane = screen.getByRole('region', { name: 'Implementer in Pad' });
	const list = within(padLane).getAllByRole('listitem').map(li => li.getAttribute('role') === 'separator' ? 'LINE' : li.textContent);
	expect(list.findIndex(t => t === 'LINE')).toBeLessThan(list.findIndex(t => t?.includes('TASK-1')));
	await fireEvent.click(screen.getByRole('button', { name: 'Actions for TASK-3' }));
	await fireEvent.click(await screen.findByRole('menuitem', { name: /Do next/ }));
	await settle();
	expect(h.reorder).toHaveBeenCalledWith('pad', [{ item_id: 'id-TASK-3', role_sort_order: 0 }]);
	expect(h.park).not.toHaveBeenCalled();
	expect(h.toast).toHaveBeenCalledWith('Queued first; Implementer is parked', 'info');
});

it('hides the line handle and item handles where the server says the caller cannot write', async () => {
	h.agents.mockResolvedValue(answer([lane({ can_park: false, queue: [item('TASK-1', 1024, false), item('TASK-2', 2048), item('TASK-3', 3072)] })]));
	await open();
	const padLane = screen.getByRole('region', { name: 'Implementer in Pad' });
	expect(within(padLane).getByRole('separator')).not.toHaveAttribute('draggable');
	expect(within(padLane).queryByRole('button', { name: /^Unpark/ })).not.toBeInTheDocument();
	expect(within(padLane).getByText('Title TASK-1').closest('li')).not.toHaveAttribute('draggable');
	expect(within(padLane).getByText('Title TASK-2').closest('li')).toHaveAttribute('draggable', 'true');
	expect(screen.queryByRole('button', { name: /Park after current/ })).not.toBeInTheDocument();
});

it('a view-only lane is read-only', async () => {
	await open();
	const cloud = screen.getByRole('region', { name: 'Implementer in Cloud' });
	expect(cloud.querySelector('[draggable="true"]')).toBeNull();
	expect(within(cloud).queryByRole('button', { name: /^Actions for/ })).not.toBeInTheDocument();
	expect(cloud).toHaveTextContent('view only');
});

it('Park after current parks every lane it may and reports the skipped ones', async () => {
	await open();
	await fireEvent.click(screen.getByRole('button', { name: /Park after current/ }));
	await settle();
	expect(h.park).toHaveBeenCalledTimes(1);
	expect(h.park).toHaveBeenCalledWith('pad', 'impl', { at: 'top', expected_park_seq: 4 });
	expect(h.toast).toHaveBeenCalledWith(expect.stringContaining('Skipped (view only): Cloud'), 'success');
});

it('a hint during a drag is deferred to the drop', async () => {
	await open();
	expect(h.agents).toHaveBeenCalledTimes(1);
	const padLane = screen.getByRole('region', { name: 'Implementer in Pad' });
	const row = within(padLane).getByText('Title TASK-2').closest('li')!;
	await fireEvent.dragStart(row);
	for (const cb of h.live) cb();
	await settle();
	expect(h.agents).toHaveBeenCalledTimes(1);
	await fireEvent.dragEnd(row);
	await settle();
	expect(h.agents).toHaveBeenCalledTimes(2);
});

it('a hint with nothing in flight reloads', async () => {
	render(AgentsPanel);
	await settle();
	for (const cb of h.live) cb();
	await settle();
	expect(h.agents).toHaveBeenCalledTimes(2);
});

it('phone: strips only, one agent open at a time, no draggable, and a row menu', async () => {
	h.mobile = true;
	h.agents.mockResolvedValue({ agents: [...answer([lane()]).agents, { ...answer([lane({ role: { slug: 'rev', name: 'Reviewer', icon: '' } })]).agents[0], slug: 'rev', name: 'Reviewer' }], degraded: [] });
	await open();
	expect(document.querySelector('[draggable="true"]')).toBeNull();
	await fireEvent.click(screen.getByRole('button', { name: 'Actions for TASK-2' }));
	const names = (await screen.findAllByRole('menuitem')).map(m => m.textContent?.trim());
	expect(names).toEqual(expect.arrayContaining([expect.stringContaining('Do next'), expect.stringContaining('Move up'), expect.stringContaining('Move down'), expect.stringContaining('Reassign'), expect.stringContaining('Park before this'), expect.stringContaining('Unpark')]));
	await fireEvent.click(screen.getByRole('button', { name: /Reviewer/, expanded: false }));
	await settle();
	expect(screen.getAllByRole('button', { name: /Implementer|Reviewer/, expanded: true })).toHaveLength(1);
});

it('an identity change closes what was open', async () => {
	await open();
	expect(screen.getByRole('region', { name: 'Implementer in Pad' })).toBeInTheDocument();
	h.epoch++; for (const r of [...h.resets]) r();
	await settle();
	expect(screen.queryByRole('region', { name: 'Implementer in Pad' })).not.toBeInTheDocument();
});
