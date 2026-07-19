import { describe, it, expect } from 'vitest';
import { computeMutationsEnabled } from './mutationGate';

// PLAN-2154 Phase 2 / D2 / R12 (TASK-2172). `computeMutationsEnabled` is the
// single source of truth for the retain-alive master freeze: `ItemDetail`'s
// `mutationsEnabled` derived and its freeze test probe both import it, so the
// gate predicate can't drift between the component and its coverage.
describe('computeMutationsEnabled — master-freeze gate predicate (TASK-2172)', () => {
	it('is exactly canEdit while NOT peeking (byte-identical for non-host callers)', () => {
		// `peeking` defaults false at every non-host call site, so the freeze
		// prop must leave the gate equal to raw `canEdit`.
		expect(computeMutationsEnabled(true, false)).toBe(true);
		expect(computeMutationsEnabled(false, false)).toBe(false);
	});

	it('is false while peeking regardless of canEdit (complete freeze)', () => {
		// The core freeze invariant: a peeking master enables NO mutation,
		// even for an owner/editor who would otherwise have full edit rights.
		expect(computeMutationsEnabled(true, true)).toBe(false);
		expect(computeMutationsEnabled(false, true)).toBe(false);
	});

	it('only ever returns true in the single (canEdit && !peeking) corner', () => {
		const truthTable: Array<[boolean, boolean, boolean]> = [
			[true, false, true],
			[true, true, false],
			[false, false, false],
			[false, true, false],
		];
		for (const [canEdit, peeking, expected] of truthTable) {
			expect(computeMutationsEnabled(canEdit, peeking)).toBe(expected);
		}
	});
});
