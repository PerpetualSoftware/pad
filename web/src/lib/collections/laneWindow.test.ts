// TASK-2230: a lane mounts a window; the full lane is what gets persisted.
import { describe, it, expect } from 'vitest';
import { LANE_CAP, TERMINAL_LANE_CAP, laneCap, laneWindow, rebuildLane } from './laneWindow';

const lane = (n: number, prefix = 'c') => Array.from({ length: n }, (_, i) => ({ id: `${prefix}${i}` }));
const ids = (xs: readonly { id: string }[]) => xs.map((x) => x.id);

describe('lane window (TASK-2230)', () => {
	it('caps terminal lanes tighter than the rest, and nothing when uncapped', () => {
		expect(laneCap({ terminal: true, uncapped: false })).toBe(TERMINAL_LANE_CAP);
		expect(laneCap({ terminal: false, uncapped: false })).toBe(LANE_CAP);
		expect(laneCap({ terminal: true, uncapped: true })).toBe(Infinity);
		expect(TERMINAL_LANE_CAP).toBeLessThan(LANE_CAP);
	});

	it('mounts the first cards of the lane, or all of a short one', () => {
		expect(ids(laneWindow(lane(5), 3))).toEqual(['c0', 'c1', 'c2']);
		const short = lane(2);
		expect(laneWindow(short, 3)).toBe(short);
	});

	it('a reorder inside the window keeps the hidden tail behind it, in order', () => {
		const full = lane(6);
		const before = laneWindow(full, 3);
		const after = [full[2], full[0], full[1]];
		expect(ids(rebuildLane(full, before, after))).toEqual(['c2', 'c0', 'c1', 'c3', 'c4', 'c5']);
	});

	it('a card dragged in lands where it was dropped, ahead of the hidden tail', () => {
		const full = lane(6);
		const before = laneWindow(full, 3);
		const incoming = { id: 'x' };
		const after = [full[0], incoming, full[1], full[2]];
		expect(ids(rebuildLane(full, before, after))).toEqual(['c0', 'x', 'c1', 'c2', 'c3', 'c4', 'c5']);
	});

	it('a card dragged out is gone, and the tail does not move up into the gap twice', () => {
		const full = lane(6);
		const before = laneWindow(full, 3);
		const after = [full[0], full[2]];
		expect(ids(rebuildLane(full, before, after))).toEqual(['c0', 'c2', 'c3', 'c4', 'c5']);
	});

	it('never duplicates or loses a card (exhaustive over small lanes)', () => {
		for (let n = 1; n <= 7; n++) {
			for (let w = 1; w <= n; w++) {
				const full = lane(n);
				const before = laneWindow(full, w);
				// Move each window card to each window position.
				for (let from = 0; from < before.length; from++) {
					for (let to = 0; to < before.length; to++) {
						const after = [...before];
						const [m] = after.splice(from, 1);
						after.splice(to, 0, m);
						const out = ids(rebuildLane(full, before, after));
						expect(new Set(out).size).toBe(n);
						expect(out.slice(w)).toEqual(ids(full.slice(w)));
					}
				}
			}
		}
	});
});
