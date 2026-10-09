import { describe, expect, it } from 'vitest';
import { planLaneOrder, persistReorder, type OrderedCard, type OrderWrite } from './reorderPlan';

const c = (id: string, sort_order: number): OrderedCard => ({ id, sort_order });

// The stored order a plan produces, read the way the lane sorts it: by value,
// frozen ties keeping their on-screen order.
function sortedAfter(lane: OrderedCard[], writes: OrderWrite[]): string[] {
	const value = new Map(lane.map((x) => [x.id, x.sort_order]));
	for (const w of writes) value.set(w.id, w.sort_order);
	return lane
		.map((x, i) => ({ id: x.id, v: value.get(x.id)!, i }))
		.sort((a, b) => a.v - b.v || a.i - b.i)
		.map((x) => x.id);
}

describe('planLaneOrder (BUG-3259, TASK-2230)', () => {
	it('a drop at the top of an all-editable lane writes the moved card alone, below the rest (TASK-2230)', () => {
		const lane = [c('c', 2), c('a', 0), c('b', 1)];
		expect(planLaneOrder(lane, () => true)).toEqual({ ok: true, writes: [{ id: 'c', sort_order: -1 }] });
	});

	it('a drop at the bottom writes the moved card alone, above the rest', () => {
		const lane = [c('b', 1), c('c', 2), c('a', 0)];
		expect(planLaneOrder(lane, () => true)).toEqual({ ok: true, writes: [{ id: 'a', sort_order: 3 }] });
	});

	it('a lane nothing moved in writes nothing', () => {
		expect(planLaneOrder([c('a', 0), c('b', 4), c('c', 9)], () => true)).toEqual({ ok: true, writes: [] });
	});

	it('a lane of ties (never ordered: every value 0) is written apart wherever the order needs it', () => {
		const lane = [c('a', 0), c('b', 0), c('c', 0)];
		const plan = planLaneOrder(lane, () => true);
		expect(plan.ok).toBe(true);
		if (plan.ok) expect(sortedAfter(lane, plan.writes)).toEqual(['a', 'b', 'c']);
	});

	it('moving one card writes at most the shorter side plus the card; an end drop writes one (exhaustive to 9)', () => {
		let checked = 0;
		for (let n = 2; n <= 9; n++) {
			for (let from = 0; from < n; from++) {
				for (let to = 0; to < n; to++) {
					if (from === to) continue;
					const ids = [...Array(n).keys()];
					const [moved] = ids.splice(from, 1);
					ids.splice(to, 0, moved);
					const lane = ids.map((orig) => c(`i${orig}`, orig));
					const plan = planLaneOrder(lane, () => true);
					expect(plan.ok).toBe(true);
					if (!plan.ok) continue;
					expect(sortedAfter(lane, plan.writes)).toEqual(lane.map((x) => x.id));
					if (to === 0 || to === n - 1) expect(plan.writes, `n=${n} ${from}->${to}`).toHaveLength(1);
					else expect(plan.writes.length, `n=${n} ${from}->${to}`).toBeLessThanOrEqual(Math.min(to, n - 1 - to) + 1);
					checked++;
				}
			}
		}
		expect(checked).toBeGreaterThan(200);
	});

	it('a middle drop in a 1,020-card lane writes about half of it, not all of it', () => {
		const n = 1020;
		const ids = [...Array(n).keys()];
		const [moved] = ids.splice(n - 1, 1);
		ids.splice(510, 0, moved);
		const plan = planLaneOrder(ids.map((o) => c(`i${o}`, o)), () => true);
		expect(plan.ok && plan.writes.length).toBeLessThanOrEqual(511);
	});

	it('never writes a view-only card', () => {
		// The measured case: C dragged above view-only A.
		const lane = [c('c', 2), c('a', 0), c('b', 1)];
		const plan = planLaneOrder(lane, (x) => x.id !== 'a');
		expect(plan.ok).toBe(true);
		if (!plan.ok) return;
		expect(plan.writes.map((w) => w.id)).not.toContain('a');
		expect(sortedAfter(lane, plan.writes)).toEqual(['c', 'a', 'b']);
	});

	it('a dense renumber that skipped the frozen card would collide; the plan does not', () => {
		// C moved to the top, A (frozen) keeps 0. Dense-minus-frozen would write
		// C=0, B=2: C and A tie at 0 and the tie-break decides, not the user.
		const lane = [c('c', 2), c('a', 0), c('b', 1)];
		const plan = planLaneOrder(lane, (x) => x.id !== 'a');
		if (!plan.ok) throw new Error('expected a plan');
		const v = new Map(lane.map((x) => [x.id, x.sort_order]));
		for (const w of plan.writes) v.set(w.id, w.sort_order);
		expect(v.get('c')!).toBeLessThan(v.get('a')!);
		expect(v.get('a')!).toBeLessThan(v.get('b')!);
	});

	it('keeps an editable card whose value already fits', () => {
		const lane = [c('a', 0), c('x', 5), c('b', 9)];
		const plan = planLaneOrder(lane, (x) => x.id === 'x');
		expect(plan).toEqual({ ok: true, writes: [] });
	});

	it('refuses when there is no integer room between frozen cards', () => {
		const lane = [c('a', 0), c('x', 7), c('b', 1)];
		expect(planLaneOrder(lane, (x) => x.id === 'x')).toEqual({ ok: false });
	});

	it('allows two frozen cards to tie when nothing moved between them', () => {
		const lane = [c('x', 3), c('a', 0), c('b', 0)];
		const plan = planLaneOrder(lane, (x) => x.id === 'x');
		expect(plan.ok).toBe(true);
		if (!plan.ok) return;
		expect(sortedAfter(lane, plan.writes)).toEqual(['x', 'a', 'b']);
	});

	it('refuses a frozen card that would have to sort before what precedes it', () => {
		const lane = [c('b', 4), c('a', 1)];
		expect(planLaneOrder(lane, () => false)).toEqual({ ok: false });
	});

	// Exhaustive over small lanes: every plan that succeeds sorts to the
	// on-screen order and never writes a frozen card.
	it('every successful plan sorts to the on-screen order (exhaustive, lanes up to 5)', () => {
		let checked = 0;
		for (let n = 1; n <= 5; n++) {
			const perms = permutations([...Array(n).keys()]);
			for (const order of perms) {
				for (let mask = 0; mask < 1 << n; mask++) {
					// Stored values are the ORIGINAL dense order; `order` is the new screen order.
					const lane = order.map((orig) => c(`i${orig}`, orig));
					const frozen = (x: OrderedCard) => (mask >> Number(x.id.slice(1))) & 1;
					const plan = planLaneOrder(lane, (x) => !frozen(x));
					if (!plan.ok) continue;
					checked++;
					for (const w of plan.writes) expect(frozen(lane.find((x) => x.id === w.id)!)).toBe(0);
					expect(sortedAfter(lane, plan.writes)).toEqual(lane.map((x) => x.id));
				}
			}
		}
		expect(checked).toBeGreaterThan(1000);
	});
});

function permutations(xs: number[]): number[][] {
	if (xs.length <= 1) return [xs];
	return xs.flatMap((x, i) => permutations([...xs.slice(0, i), ...xs.slice(i + 1)]).map((p) => [x, ...p]));
}

describe('persistReorder (BUG-3259, TASK-3517)', () => {
	function harness(refuse: boolean) {
		const originals = new Map([
			['a', { id: 'a', sort_order: 0, seq: 10 }],
			['b', { id: 'b', sort_order: 1, seq: 11 }],
			['c', { id: 'c', sort_order: 2, seq: 12 }]
		]);
		const local = new Map([...originals].map(([k, v]) => [k, { ...v }]));
		const requests: OrderWrite[][] = [];
		return {
			local,
			requests,
			deps: {
				original: (id: string) => originals.get(id),
				applyLocal: (card: { id: string; sort_order: number; seq?: number }) =>
					local.set(card.id, { ...card, seq: undefined as unknown as number }),
				restoreLocal: (card: { id: string; sort_order: number; seq: number }) => local.set(card.id, { ...card }),
				send: async (ws: OrderWrite[]) => {
					requests.push(ws);
					if (refuse) throw new Error('403');
					return ws.map((w) => ({ id: w.id, seq: originals.get(w.id)!.seq + 10 }));
				},
				settle: (row: { id: string; sort_order: number; seq: number }) => {
					local.set(row.id, row);
					return true;
				}
			}
		};
	}
	const writes = [
		{ id: 'c', sort_order: 0 },
		{ id: 'a', sort_order: 1 },
		{ id: 'b', sort_order: 2 }
	];

	it('persists every write in ONE request and settles the returned seqs', async () => {
		const h = harness(false);
		expect(await persistReorder(writes, h.deps)).toBe(true);
		expect(h.requests).toEqual([writes]);
		expect([...h.local.values()].map((r) => [r.id, r.sort_order, r.seq])).toEqual([
			['a', 1, 20],
			['b', 2, 21],
			['c', 0, 22]
		]);
	});

	it('a refusal restores EVERY card: the server wrote nothing', async () => {
		const h = harness(true);
		expect(await persistReorder(writes, h.deps)).toBe(false);
		expect(h.requests).toHaveLength(1);
		expect(h.local.get('c')).toEqual({ id: 'c', sort_order: 2, seq: 12 });
		expect(h.local.get('a')).toEqual({ id: 'a', sort_order: 0, seq: 10 });
		expect(h.local.get('b')).toEqual({ id: 'b', sort_order: 1, seq: 11 });
	});

	it('sends nothing when no write names a known card', async () => {
		const h = harness(false);
		expect(await persistReorder([{ id: 'zz', sort_order: 3 }], h.deps)).toBe(true);
		expect(h.requests).toHaveLength(0);
	});
});
