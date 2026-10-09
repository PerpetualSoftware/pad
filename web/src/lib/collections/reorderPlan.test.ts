import { describe, expect, it } from 'vitest';
import { planLaneOrder, persistReorder, SORT_GAP, SORT_LIMIT, type OrderedCard, type OrderWrite } from './reorderPlan';

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
		expect(planLaneOrder(lane, () => true)).toEqual({ ok: true, writes: [{ id: 'c', sort_order: -SORT_GAP }] });
	});

	it('a drop at the bottom writes the moved card alone, above the rest', () => {
		const lane = [c('b', 1), c('c', 2), c('a', 0)];
		expect(planLaneOrder(lane, () => true)).toEqual({ ok: true, writes: [{ id: 'a', sort_order: 2 + SORT_GAP }] });
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

	// TASK-3525: a gapped lane moves one card with ONE write.
	it('in a gapped lane every single move is one write (exhaustive to 9)', () => {
		let checked = 0;
		for (let n = 2; n <= 9; n++) {
			for (let from = 0; from < n; from++) {
				for (let to = 0; to < n; to++) {
					if (from === to) continue;
					const ids = [...Array(n).keys()];
					const [moved] = ids.splice(from, 1);
					ids.splice(to, 0, moved);
					const lane = ids.map((orig) => c(`i${orig}`, orig * SORT_GAP));
					const plan = planLaneOrder(lane, () => true);
					if (!plan.ok) throw new Error('expected a plan');
					// One write. Usually the moved card; in an adjacent swap either card
					// can be the one written, which is the same cost.
					expect(plan.writes, `n=${n} ${from}->${to}`).toHaveLength(1);
					if (Math.abs(from - to) > 1) expect(plan.writes[0].id).toBe(`i${moved}`);
					expect(sortedAfter(lane, plan.writes)).toEqual(lane.map((x) => x.id));
					checked++;
				}
			}
		}
		expect(checked).toBeGreaterThan(200);
	});

	it('a middle drop in a gapped 1,020-card lane is one write', () => {
		const n = 1020;
		const ids = [...Array(n).keys()];
		const [moved] = ids.splice(n - 1, 1);
		ids.splice(510, 0, moved);
		const plan = planLaneOrder(ids.map((o) => c(`i${o}`, o * SORT_GAP)), () => true);
		expect(plan.ok && plan.writes).toHaveLength(1);
	});

	// Drops into one slot halve its gap; when it closes, one bounded shift
	// re-spaces, and the drops after it are single writes again.
	it('repeated drops into one slot stay cheap', () => {
		let lane = [...Array(20).keys()].map((o) => c(`i${o}`, o * SORT_GAP));
		const counts: number[] = [];
		for (let drop = 0; drop < 30; drop++) {
			// Take the last card and drop it right after the first.
			const moved = lane[lane.length - 1];
			const next = [lane[0], moved, ...lane.slice(1, -1)];
			const plan = planLaneOrder(next, () => true);
			if (!plan.ok) throw new Error('expected a plan');
			expect(sortedAfter(next, plan.writes)).toEqual(next.map((x) => x.id));
			counts.push(plan.writes.length);
			const value = new Map(next.map((x) => [x.id, x.sort_order]));
			for (const w of plan.writes) value.set(w.id, w.sort_order);
			lane = next.map((x) => c(x.id, value.get(x.id)!));
		}
		// 30 drops into one slot of a 20-card lane: mostly single writes, and the
		// re-spaces are bounded by the shorter side (here the one card before it).
		expect(counts.filter((x) => x === 1).length).toBeGreaterThan(20);
		expect(Math.max(...counts)).toBeLessThanOrEqual(10);
	});

	it('a never-dragged lane (all 0) pays once: the next move is one write', () => {
		const lane = [...Array(8).keys()].map((o) => c(`i${o}`, 0));
		const first = [lane[3], ...lane.slice(0, 3), ...lane.slice(4)];
		const plan1 = planLaneOrder(first, () => true);
		if (!plan1.ok) throw new Error('expected a plan');
		expect(sortedAfter(first, plan1.writes)).toEqual(first.map((x) => x.id));
		const value = new Map(first.map((x) => [x.id, x.sort_order]));
		for (const w of plan1.writes) value.set(w.id, w.sort_order);
		const spaced = first.map((x) => c(x.id, value.get(x.id)!));
		const second = [spaced[0], spaced[5], ...spaced.slice(1, 5), ...spaced.slice(6)];
		const plan2 = planLaneOrder(second, () => true);
		expect(plan2.ok && plan2.writes).toHaveLength(1);
	});

	it('a plan that would leave the range re-spaces the whole lane around 0', () => {
		const lane = [c('b', SORT_LIMIT - 10), c('c', SORT_LIMIT), c('a', 0)];
		// Moving a to the bottom would write SORT_LIMIT + SORT_GAP.
		const plan = planLaneOrder(lane, () => true);
		if (!plan.ok) throw new Error('expected a plan');
		for (const w of plan.writes) expect(Math.abs(w.sort_order)).toBeLessThanOrEqual(SORT_LIMIT);
		expect(sortedAfter(lane, plan.writes)).toEqual(['b', 'c', 'a']);
	});

	it('with a view-only card present, a moved card still gets room, not prev + 1', () => {
		// x (editable) dropped below frozen b: it gets b + SORT_GAP.
		const lane = [c('a', 0), c('b', SORT_GAP), c('x', -5)];
		const plan = planLaneOrder(lane, (y) => y.id === 'x');
		expect(plan).toEqual({ ok: true, writes: [{ id: 'x', sort_order: 2 * SORT_GAP }] });
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
