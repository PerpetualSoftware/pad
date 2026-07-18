import { describe, it, expect } from 'vitest';
import {
	readPaneState,
	planPaneDrill,
	planLateralOpen,
	planPaneClose,
	PANE_DEPTH_SOFT_CAP,
	type ResolvedPaneState,
} from './paneController';

const base: ResolvedPaneState = { paneDepth: 0, paneOwned: false };
const ownedBase: ResolvedPaneState = { paneDepth: 0, paneOwned: true };

describe('readPaneState', () => {
	it('defaults an unstamped (cold-loaded) entry to depth 0, UNOWNED', () => {
		expect(readPaneState(undefined)).toEqual({ paneDepth: 0, paneOwned: false });
		expect(readPaneState(null)).toEqual({ paneDepth: 0, paneOwned: false });
		expect(readPaneState({})).toEqual({ paneDepth: 0, paneOwned: false });
	});

	it('reads a full stamp verbatim', () => {
		expect(readPaneState({ paneDepth: 3, paneOwned: true })).toEqual({
			paneDepth: 3,
			paneOwned: true,
		});
	});

	it('coerces owned to a strict boolean (only literal true owns)', () => {
		// eslint-disable-next-line @typescript-eslint/no-explicit-any
		expect(readPaneState({ paneOwned: 1 as any }).paneOwned).toBe(false);
		// eslint-disable-next-line @typescript-eslint/no-explicit-any
		expect(readPaneState({ paneOwned: 'yes' as any }).paneOwned).toBe(false);
		expect(readPaneState({ paneOwned: true }).paneOwned).toBe(true);
	});

	it('floors negative / fractional / non-numeric depths to 0', () => {
		expect(readPaneState({ paneDepth: -4 }).paneDepth).toBe(0);
		expect(readPaneState({ paneDepth: 2.9 }).paneDepth).toBe(2);
		// eslint-disable-next-line @typescript-eslint/no-explicit-any
		expect(readPaneState({ paneDepth: 'x' as any }).paneDepth).toBe(0);
		expect(readPaneState({ paneDepth: NaN }).paneDepth).toBe(0);
		expect(readPaneState({ paneDepth: Infinity }).paneDepth).toBe(0);
	});
});

describe('planPaneDrill — same-ref guard (D4)', () => {
	it('is a no-op when the target equals the currently-shown item', () => {
		expect(planPaneDrill('TASK-5', 'TASK-5', ownedBase)).toEqual({ kind: 'noop' });
		expect(planPaneDrill('TASK-5', 'TASK-5', { paneDepth: 4, paneOwned: true })).toEqual({
			kind: 'noop',
		});
	});

	it('is a no-op for a falsy / empty target', () => {
		expect(planPaneDrill('TASK-5', null, ownedBase)).toEqual({ kind: 'noop' });
		expect(planPaneDrill('TASK-5', undefined, ownedBase)).toEqual({ kind: 'noop' });
		expect(planPaneDrill('TASK-5', '', ownedBase)).toEqual({ kind: 'noop' });
	});

	it('drills when the target differs, even if currentRef is null (cold base)', () => {
		expect(planPaneDrill(null, 'TASK-9', base)).toEqual({
			kind: 'push',
			state: { paneDepth: 1, paneOwned: false },
		});
	});
});

describe('planPaneDrill — ownership INHERITANCE', () => {
	it('a drill from an OWNED base inherits owned=true', () => {
		expect(planPaneDrill('TASK-1', 'TASK-2', ownedBase)).toEqual({
			kind: 'push',
			state: { paneDepth: 1, paneOwned: true },
		});
	});

	it('a drill from a COLD (unowned) base inherits owned=false — keeps the cold-close branch reachable', () => {
		expect(planPaneDrill('TASK-1', 'TASK-2', base)).toEqual({
			kind: 'push',
			state: { paneDepth: 1, paneOwned: false },
		});
	});

	it('inheritance carries through multiple hops', () => {
		expect(planPaneDrill('TASK-2', 'TASK-3', { paneDepth: 1, paneOwned: false })).toEqual({
			kind: 'push',
			state: { paneDepth: 2, paneOwned: false },
		});
		expect(planPaneDrill('TASK-3', 'TASK-4', { paneDepth: 2, paneOwned: true })).toEqual({
			kind: 'push',
			state: { paneDepth: 3, paneOwned: true },
		});
	});
});

describe('planPaneDrill — soft depth cap (D4)', () => {
	it('pushes below the cap', () => {
		const belowCap = { paneDepth: PANE_DEPTH_SOFT_CAP - 1, paneOwned: true };
		expect(planPaneDrill('A', 'B', belowCap)).toEqual({
			kind: 'push',
			state: { paneDepth: PANE_DEPTH_SOFT_CAP, paneOwned: true },
		});
	});

	it('REPLACES at the cap — holds depth + ownership steady', () => {
		const atCap = { paneDepth: PANE_DEPTH_SOFT_CAP, paneOwned: true };
		expect(planPaneDrill('A', 'B', atCap)).toEqual({
			kind: 'replace',
			state: { paneDepth: PANE_DEPTH_SOFT_CAP, paneOwned: true },
		});
	});

	it('REPLACES past the cap too (no unbounded growth)', () => {
		const pastCap = { paneDepth: PANE_DEPTH_SOFT_CAP + 5, paneOwned: false };
		expect(planPaneDrill('A', 'B', pastCap)).toEqual({
			kind: 'replace',
			state: { paneDepth: PANE_DEPTH_SOFT_CAP + 5, paneOwned: false },
		});
	});

	it('honours a custom cap', () => {
		expect(planPaneDrill('A', 'B', { paneDepth: 2, paneOwned: true }, 2)).toEqual({
			kind: 'replace',
			state: { paneDepth: 2, paneOwned: true },
		});
		expect(planPaneDrill('A', 'B', { paneDepth: 1, paneOwned: true }, 2)).toEqual({
			kind: 'push',
			state: { paneDepth: 2, paneOwned: true },
		});
	});
});

describe('planLateralOpen', () => {
	it('first-open (pane closed) PUSHES and MINTS ownership', () => {
		expect(planLateralOpen(false, base)).toEqual({
			kind: 'push',
			state: { paneDepth: 0, paneOwned: true },
		});
	});

	it('re-target at depth 0 from an OWNED base REPLACES, keeping owned=true', () => {
		expect(planLateralOpen(true, ownedBase)).toEqual({
			kind: 'replace',
			state: { paneDepth: 0, paneOwned: true },
		});
	});

	it('re-target at depth 0 from a COLD base REPLACES, keeping owned=false', () => {
		expect(planLateralOpen(true, base)).toEqual({
			kind: 'replace',
			state: { paneDepth: 0, paneOwned: false },
		});
	});

	it('a direct row click at depth>0 (detached) RESETS the stack, preserving base ownership', () => {
		expect(planLateralOpen(true, { paneDepth: 3, paneOwned: true })).toEqual({
			kind: 'reset',
			goDelta: -3,
			resetState: { paneDepth: 0, paneOwned: true },
		});
		expect(planLateralOpen(true, { paneDepth: 2, paneOwned: false })).toEqual({
			kind: 'reset',
			goDelta: -2,
			resetState: { paneDepth: 0, paneOwned: false },
		});
	});
});

describe('planPaneClose — three-way staged unwind (R8)', () => {
	it('OWNED depth 0 (click-opened, no drill) → go(-1) back to the pre-pane URL', () => {
		expect(planPaneClose(ownedBase)).toEqual({ kind: 'owned-go', goDelta: -1 });
	});

	it('OWNED depth N → go(-(N+1)) unwinds base + every drill', () => {
		expect(planPaneClose({ paneDepth: 3, paneOwned: true })).toEqual({
			kind: 'owned-go',
			goDelta: -4,
		});
	});

	it('UNOWNED depth 0 (cold load) → replaceState-delete in place (never go off the base)', () => {
		expect(planPaneClose(base)).toEqual({ kind: 'replace-delete' });
	});

	it('UNOWNED depth>0 (cold base then drilled) → go(-depth) to the cold base, then latched delete', () => {
		expect(planPaneClose({ paneDepth: 2, paneOwned: false })).toEqual({
			kind: 'cold-base-go',
			goDelta: -2,
		});
		expect(planPaneClose({ paneDepth: 1, paneOwned: false })).toEqual({
			kind: 'cold-base-go',
			goDelta: -1,
		});
	});
});

describe('integration — full open/drill/close round-trips', () => {
	it('click-open → close returns to the pre-pane URL (owned go(-1))', () => {
		const open = planLateralOpen(false, base); // push {0,true}
		expect(open).toMatchObject({ kind: 'push', state: { paneDepth: 0, paneOwned: true } });
		const close = planPaneClose({ paneDepth: 0, paneOwned: true });
		expect(close).toEqual({ kind: 'owned-go', goDelta: -1 });
	});

	it('click-open → drill A→B→C → close unwinds all four entries', () => {
		// first-open A: {0,true}
		let s = readPaneState({ paneDepth: 0, paneOwned: true });
		// drill A→B
		const b = planPaneDrill('A', 'B', s);
		expect(b).toMatchObject({ kind: 'push', state: { paneDepth: 1, paneOwned: true } });
		s = readPaneState((b as { state: ResolvedPaneState }).state);
		// drill B→C
		const c = planPaneDrill('B', 'C', s);
		expect(c).toMatchObject({ kind: 'push', state: { paneDepth: 2, paneOwned: true } });
		s = readPaneState((c as { state: ResolvedPaneState }).state);
		// close from depth 2 owned → go(-3)
		expect(planPaneClose(s)).toEqual({ kind: 'owned-go', goDelta: -3 });
	});

	it('cold-load A → drill A→B → close goes to the cold base then deletes (NOT go(-2) off the base)', () => {
		// cold load: unstamped → {0,false}
		let s = readPaneState(undefined);
		expect(s).toEqual({ paneDepth: 0, paneOwned: false });
		const b = planPaneDrill('A', 'B', s);
		expect(b).toMatchObject({ kind: 'push', state: { paneDepth: 1, paneOwned: false } });
		s = readPaneState((b as { state: ResolvedPaneState }).state);
		// close from depth 1 UNOWNED → go(-1) to cold base, then latched delete
		expect(planPaneClose(s)).toEqual({ kind: 'cold-base-go', goDelta: -1 });
	});

	it('detached row click resets, and the reset base then closes correctly', () => {
		// owned, drilled to depth 2
		const reset = planLateralOpen(true, { paneDepth: 2, paneOwned: true });
		expect(reset).toEqual({
			kind: 'reset',
			goDelta: -2,
			resetState: { paneDepth: 0, paneOwned: true },
		});
		// after reset the base is {0,true}; closing it is a clean go(-1)
		expect(planPaneClose((reset as { resetState: ResolvedPaneState }).resetState)).toEqual({
			kind: 'owned-go',
			goDelta: -1,
		});
	});
});
