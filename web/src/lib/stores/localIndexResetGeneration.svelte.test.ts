// `resetGenerationFor` (TASK-2877): the identity signal a caller outside this
// module needs to tell "still the state I was authorized against" from "a fresh
// state that has reached the same numbers".
//
// The properties under test are exactly the ones the two existing counters do
// NOT have, which is why this had to be new surface rather than a reuse:
// `scopeEpoch` lives on the state object and `reset()` deletes that object, so
// the replacement starts at 0 — and 0 is also the value whenever no projection
// resync has ever run, i.e. the ordinary case. Comparing it across a reset
// therefore compares two different identities that agree by default.
import { afterEach, describe, expect, it } from 'vitest';
import { localIndex } from './localIndex.svelte';
import type { ItemIndexRow } from '$lib/types';

const ws = 'reset-generation-test';

function row(id: string, seq: number): ItemIndexRow {
	return {
		id,
		seq,
		collection_slug: 'colors',
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
	} as ItemIndexRow;
}

afterEach(() => {
	localIndex.reset(ws);
});

describe('localIndex.resetGenerationFor', () => {
	it('is 0 for a workspace it has never seen', () => {
		expect(localIndex.resetGenerationFor('never-touched-workspace')).toBe(0);
	});

	it('advances on every reset and does NOT restart with the new state', () => {
		const before = localIndex.resetGenerationFor(ws);
		localIndex.upsert(ws, row('a', 1));
		expect(localIndex.resetGenerationFor(ws)).toBe(before);

		localIndex.reset(ws);
		const afterFirst = localIndex.resetGenerationFor(ws);
		expect(afterFirst).toBe(before + 1);

		// The point of the whole accessor: the workspace is live again, and the
		// counter has NOT gone back to where it was.
		localIndex.upsert(ws, row('b', 1));
		expect(localIndex.resetGenerationFor(ws)).toBe(afterFirst);

		localIndex.reset(ws);
		expect(localIndex.resetGenerationFor(ws)).toBe(before + 2);
	});

	it('advances even when the reset found no state to drop', () => {
		// A purge racing a first bootstrap must not read as "no purge happened".
		const fresh = 'reset-generation-test-unseen';
		const before = localIndex.resetGenerationFor(fresh);
		localIndex.reset(fresh);
		expect(localIndex.resetGenerationFor(fresh)).toBe(before + 1);
	});

	it('CONTROL: scopeEpochFor cannot answer this question', () => {
		// Not a test of the store so much as of the premise the fence rests on.
		// If this ever stops holding, the caller-side equality check on
		// `scopeEpoch` would have been sufficient after all.
		localIndex.upsert(ws, row('a', 1));
		const epochBefore = localIndex.scopeEpochFor(ws);
		localIndex.reset(ws);
		localIndex.upsert(ws, row('b', 1));
		expect(localIndex.scopeEpochFor(ws)).toBe(epochBefore);
		expect(localIndex.resetGenerationFor(ws)).not.toBe(0);
	});
});
