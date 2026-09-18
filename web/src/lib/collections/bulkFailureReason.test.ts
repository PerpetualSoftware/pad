// BUG-3102: the collection page dropped the server's reason for a refused
// restore. These cover the sentence builder; the page's use of it is covered
// by the mounted legs in the collection route's own suite.
//
// Every expectation here is a LITERAL. Re-deriving the string with the same
// template the implementation uses would pass even if the template changed,
// which is the failure mode that makes a wording test worthless.
import { describe, it, expect } from 'vitest';
import { summarizeBulkFailures, bulkToastMessage } from './bulkFailureReason';
import type { BulkItemFailure } from '$lib/types';

const PLAN_LIMIT = "You've reached the 100-item limit on the free plan.";

function row(over: Partial<BulkItemFailure> = {}): BulkItemFailure {
	return { ref: 'TASK-1', error: PLAN_LIMIT, code: 'plan_limit_exceeded', ...over };
}

describe('summarizeBulkFailures', () => {
	it('groups rows sharing a reason into one clause carrying the count', () => {
		const s = summarizeBulkFailures([row({ ref: 'TASK-1' }), row({ ref: 'TASK-2' }), row({ ref: 'TASK-3' })], 0);
		expect(s.total).toBe(3);
		expect(s.reasons).toEqual([`3 × ${PLAN_LIMIT}`]);
		expect(s.unexplained).toBe(0);
	});

	it('does not add a count to a reason that occurred once', () => {
		const s = summarizeBulkFailures([row()], 0);
		expect(s.reasons).toEqual([PLAN_LIMIT]);
	});

	it('keeps distinct reasons separate, in first-seen order', () => {
		const s = summarizeBulkFailures(
			[
				row({ ref: 'TASK-1', error: 'item not found or not archived', code: undefined }),
				row({ ref: 'TASK-2' }),
				row({ ref: 'TASK-3', error: 'item not found or not archived', code: undefined })
			],
			0
		);
		expect(s.reasons).toEqual(['2 × item not found or not archived', PLAN_LIMIT]);
	});

	it('groups by (code, message) so one code with different specifics does not collapse', () => {
		// Two open_children rows naming different blockers. Collapsing them
		// would state one row's specifics over the other's.
		const s = summarizeBulkFailures(
			[
				row({ ref: 'TASK-1', code: 'open_children', error: 'TASK-1 has 2 open children' }),
				row({ ref: 'TASK-2', code: 'open_children', error: 'TASK-2 has 5 open children' })
			],
			0
		);
		expect(s.reasons).toEqual(['TASK-1 has 2 open children', 'TASK-2 has 5 open children']);
	});

	it('ignores a row with no message rather than emitting an empty clause', () => {
		const s = summarizeBulkFailures([row({ error: '' }), row({ error: '   ' })], 0);
		expect(s.reasons).toEqual([]);
		// They still COUNT as failures — they failed, we just cannot say why.
		expect(s.total).toBe(2);
	});

	it('counts unexplained failures without attributing a reason to them', () => {
		const s = summarizeBulkFailures([row()], 3);
		expect(s.total).toBe(4);
		expect(s.unexplained).toBe(3);
		expect(s.reasons).toEqual([PLAN_LIMIT]);
	});
});

describe('bulkToastMessage', () => {
	it('all refused: leads with the failure and names the reason', () => {
		const s = summarizeBulkFailures([row({ ref: 'A' }), row({ ref: 'B' }), row({ ref: 'C' })], 0);
		expect(bulkToastMessage('Restored', 0, s)).toBe(
			`3 items could not be restored — 3 × ${PLAN_LIMIT}`
		);
	});

	it('all refused, single item: singular noun', () => {
		const s = summarizeBulkFailures([row()], 0);
		expect(bulkToastMessage('Restored', 0, s)).toBe(`1 item could not be restored — ${PLAN_LIMIT}`);
	});

	it('mixed: says how many succeeded, how many did not, and why', () => {
		const s = summarizeBulkFailures([row({ ref: 'A' }), row({ ref: 'B' }), row({ ref: 'C' })], 0);
		expect(bulkToastMessage('Restored', 2, s)).toBe(
			`Restored 2 items, 3 failed — 3 × ${PLAN_LIMIT}`
		);
	});

	it('all succeeded: no failure clause at all', () => {
		const s = summarizeBulkFailures([], 0);
		expect(bulkToastMessage('Restored', 5, s)).toBe('Restored 5 items');
	});

	it('all succeeded but the sync lagged: keeps the existing updating hint', () => {
		const s = summarizeBulkFailures([], 0);
		expect(bulkToastMessage('Restored', 5, s, { synced: false })).toBe('Restored 5 items (updating…)');
	});

	it('a thrown chunk is reported as not attempted, never as the reason', () => {
		const s = summarizeBulkFailures([row()], 2);
		const msg = bulkToastMessage('Restored', 1, s);
		expect(msg).toBe(`Restored 1 item, 3 failed — ${PLAN_LIMIT}; 2 items not attempted`);
		// The discriminating assertion: the un-attempted rows must not be
		// folded into the plan-limit count, which would tell the user three
		// items hit the cap when only one did.
		expect(msg).not.toContain(`3 × ${PLAN_LIMIT}`);
	});

	it('everything failed with no rows at all: says so without inventing a reason', () => {
		const s = summarizeBulkFailures([], 4);
		expect(bulkToastMessage('Restored', 0, s)).toBe('4 items could not be restored — 4 items not attempted');
	});

	it('composes with a two-word past-tense verb', () => {
		// "Moved back" is a live verb at a call site; the old all-failed branch
		// rendered "Failed to moved back items".
		const s = summarizeBulkFailures([row({ error: 'nope', code: undefined })], 0);
		expect(bulkToastMessage('Moved back', 0, s)).toBe('1 item could not be moved back — nope');
	});
});

describe('bulkToastMessage — the thrown chunk keeps its own error', () => {
	it('names the chunk failure as the reason for the un-attempted rows only', () => {
		const s = summarizeBulkFailures([row()], 2);
		const msg = bulkToastMessage('Restored', 1, s, { notAttemptedReason: 'Network request failed' });
		expect(msg).toBe(
			`Restored 1 item, 3 failed — ${PLAN_LIMIT}; 2 items not attempted: Network request failed`
		);
	});

	it('does not lose the chunk error when nothing succeeded and no row came back', () => {
		// The pre-BUG-3102 behaviour for this case was to show the chunk error
		// as the entire toast. It must still reach the user.
		const s = summarizeBulkFailures([], 3);
		expect(bulkToastMessage('Restored', 0, s, { notAttemptedReason: 'Network request failed' })).toBe(
			'3 items could not be restored — 3 items not attempted: Network request failed'
		);
	});

	it('omits the colon clause when there is no chunk error to report', () => {
		const s = summarizeBulkFailures([], 2);
		expect(bulkToastMessage('Restored', 0, s, { notAttemptedReason: '   ' })).toBe(
			'2 items could not be restored — 2 items not attempted'
		);
	});
});
