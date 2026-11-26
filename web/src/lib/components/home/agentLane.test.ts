import { expect, it } from 'vitest';
import { boundLane, doNextOrder, laneView, lineLabel, lineRequest, parkedLabel, planLane, stepOrder } from './agentLane';
import type { RoleQueueResponse } from '$lib/types';

const item = (ref: string, sort: number, can_edit = true) => ({ id: `id-${ref}`, ref, title: ref, status: 'open', priority: '', role_sort_order: sort, blocked: false, ready: true, can_edit });
const lane = (park?: RoleQueueResponse['park'], over: Partial<RoleQueueResponse> = {}): RoleQueueResponse => ({
	workspace: 'ws', role: { slug: 'impl', name: 'Implementer', icon: '' }, can_reorder: true, can_park: true, now: [],
	queue: [item('A', 1024), item('B', 2048), item('C', 3072)], park, queue_total: 3, parked_total: 0, queue_more: false, ...over
});
const linePark = (index: number | null, sort_order = 3072) => ({ mode: 'line' as const, sort_order, index, first_parked_ref: null, set_by: 'Dave', set_at: '2026-11-26T15:04:00Z', park_seq: 4 });
const topPark = { mode: 'top' as const, sort_order: null, index: 0, first_parked_ref: 'A', set_by: 'Dave', set_at: null, park_seq: 2 };

it('Do next puts the item first and writes it below the first item', () => {
	const l = lane();
	const next = doNextOrder(laneView(l), 'C');
	expect(next).toEqual({ refs: ['C', 'A', 'B'], line: null });
	expect(planLane(l, next, 'C', null)).toEqual({ ok: true, writes: [{ id: 'C', sort_order: 0 }] });
});

it('Do next never crosses a top line', () => {
	const l = lane(topPark);
	const next = doNextOrder(laneView(l), 'C');
	expect(next).toEqual({ refs: ['C', 'A', 'B'], line: 0 });
	expect(planLane(l, next, 'C', null)).toEqual({ ok: true, writes: [{ id: 'C', sort_order: 0 }] });
	expect(stepOrder(laneView(l), 'A', -1)).toBeNull();
});

it('a line is an immovable card: an item moved below it is written at or past the line', () => {
	const l = lane(linePark(2));
	// Line before C (P = 3072). Move A down past the line.
	const next = { refs: ['B', 'C', 'A'], line: 1 };
	const plan = planLane(l, next, 'A', 3072);
	expect(plan.ok).toBe(true);
	const a = plan.ok ? plan.writes.find(w => w.id === 'A')!.sort_order : 0;
	expect(a).toBeGreaterThanOrEqual(3072);
});

it('the line request names the item below the line, or the last item at the end', () => {
	expect(lineRequest(['A', 'B', 'C'], 1)).toEqual({ before: 'B' });
	expect(lineRequest(['A', 'B', 'C'], 3)).toEqual({ after: 'C' });
	expect(lineRequest([], 0)).toBeNull();
});

it('places the server line in the prefix, and hides one past a partial read', () => {
	expect(laneView(lane(linePark(2))).line).toBe(2);
	expect(laneView(lane(linePark(null))).line).toBe(3);
	expect(laneView(lane(linePark(null), { queue_more: true })).line).toBeNull();
	expect(laneView(lane(topPark)).line).toBe(0);
});

it('labels parked lanes and the line', () => {
	expect(parkedLabel([lane(), lane(topPark), lane(linePark(1))])).toBe('parked 2/3');
	expect(parkedLabel([lane(topPark)])).toBe('parked');
	expect(parkedLabel([lane()])).toBe('');
	expect(lineLabel('Dave', '2026-11-26T15:04:00Z', 'UTC', new Date('2026-11-26T18:00:00Z'))).toBe('Stop here. Set by Dave 15:04');
});

it('a truncated lane bounds the last shown slot by the first item not loaded', () => {
	const l = lane(undefined, { queue_more: true, queue_total: 4 });
	const hidden = item('D', 3100);
	const next = { refs: ['B', 'C', 'A'], line: null };
	const plan = planLane(l, next, 'A', null, hidden);
	expect(plan.ok).toBe(true);
	const a = plan.ok ? plan.writes.find(w => w.id === 'A')!.sort_order : Infinity;
	expect(a).toBeGreaterThan(3072);
	expect(a).toBeLessThan(3100);
});

it('a truncated lane with its line hidden keeps the last shown slot above the line', () => {
	// The line (P = 3080) sits past the loaded prefix, between C and the hidden D.
	const l = lane(linePark(null), { queue_more: true, queue_total: 4 });
	const hidden = item('D', 3500);
	const next = { refs: ['B', 'C', 'A'], line: null };
	const plan = planLane(l, next, 'A', 3080, hidden);
	expect(plan.ok).toBe(true);
	const a = plan.ok ? plan.writes.find(w => w.id === 'A')!.sort_order : Infinity;
	expect(a).toBeGreaterThan(3072);
	expect(a).toBeLessThan(3080);
	// The bound alone also holds when the hidden item sits before the line.
	const below = planLane(l, next, 'A', 4000, item('D', 3090));
	const b = below.ok ? below.writes.find(w => w.id === 'A')!.sort_order : Infinity;
	expect(b).toBeLessThan(3090);
});

it('a fresh read bounds the lane with its next item and prefers its values; a changed prefix refuses', () => {
	const l = lane(undefined, { queue_more: true, queue_total: 4 });
	const fresh = lane(undefined, { queue: [item('A', 1000), item('B', 2048), item('C', 3072), item('D', 3100)], queue_more: false, queue_total: 4 });
	const bounded = boundLane(l, fresh);
	expect(bounded.ok && bounded.bound?.ref).toBe('D');
	expect(bounded.ok && bounded.lane.queue.map(i => i.role_sort_order)).toEqual([1000, 2048, 3072]);
	const moved = lane(undefined, { queue: [item('B', 1000), item('A', 2048), item('C', 3072), item('D', 3100)] });
	expect(boundLane(l, moved)).toEqual({ ok: false });
	expect(boundLane(l, lane(undefined, { queue: [item('A', 1024), item('B', 2048)] }))).toEqual({ ok: false });
});

it('boundLane takes the fresh lane park so write-time sort_order is current', () => {
	const stalePark = linePark(2, 3072);
	const freshPark = { ...linePark(2, 3500), park_seq: 5 };
	const l = lane(stalePark, { queue_more: true, queue_total: 4 });
	const fresh = lane(freshPark, { queue: [item('A', 1024), item('B', 2048), item('C', 3500), item('D', 4000)], queue_more: false, queue_total: 4 });
	const bounded = boundLane(l, fresh);
	expect(bounded.ok).toBe(true);
	expect(bounded.ok && bounded.lane.park).toEqual(freshPark);
	const unparked = boundLane(l, lane(undefined, { queue: [item('A', 1024), item('B', 2048), item('C', 3072), item('D', 4000)], queue_more: false, queue_total: 4 }));
	expect(unparked.ok).toBe(true);
	expect(unparked.ok && unparked.lane.park).toBeUndefined();
});
