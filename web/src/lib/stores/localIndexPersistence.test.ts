import { describe, it, expect } from 'vitest';
import { shouldApplyRetag, shouldWriteRow } from './localIndexPersistence';
import type { ItemIndexRow } from '$lib/types';

/**
 * BUG-2609 — the IDB write path had no seq guard, so a snapshot taken from RAM
 * and persisted fire-and-forget could land AFTER a newer delta's atomic
 * rows+cursor transaction. The cache then held a pre-delta row while the
 * persisted cursor sat past that delta: warm boot hydrates the stale row and
 * `/items-changes?since=cursor` never returns it, so it stays stale until the
 * item happens to change again.
 *
 * These cover the DECISION. The interleaving itself is not covered — see the
 * note at the bottom of this file, which says so rather than leaving a reader
 * to assume otherwise.
 */

function row(seq: number | undefined, id = 'item-1'): ItemIndexRow {
	return { id, seq } as unknown as ItemIndexRow;
}

function collRow(collectionId: string, slug: string, id = 'item-1'): ItemIndexRow {
	return { id, collection_id: collectionId, collection_slug: slug } as unknown as ItemIndexRow;
}

describe('shouldWriteRow', () => {
	it('writes when nothing is stored yet', () => {
		expect(shouldWriteRow(undefined, row(5))).toBe(true);
	});

	it('REFUSES a strictly older row — the whole point of the guard', () => {
		expect(shouldWriteRow(row(7), row(6))).toBe(false);
		expect(shouldWriteRow(row(7), row(0))).toBe(false);
	});

	it('writes a newer row', () => {
		expect(shouldWriteRow(row(6), row(7))).toBe(true);
	});

	// Equal seq must still write. RAM merges same-seq projections before
	// persisting (localIndex's mergeRow), so the incoming row IS the merged
	// one; refusing it would drop that merge and leave the cache behind RAM
	// with no seq difference to ever correct it.
	// Within a tab this is right: upsert returns early on equal seq, and any
	// row that does reach here has been through RAM's projection merge.
	// Across tabs it leaves a residual (BUG-2635) — accept-or-refuse is the
	// wrong axis at equal seq, and a merge is what this layer lacks.
	it('writes on equal seq, because RAM has already merged the projection', () => {
		expect(shouldWriteRow(row(7), row(7))).toBe(true);
	});

	// Missing seq is ASYMMETRIC. An incoming row that carries ordering
	// evidence beats a stored row that has none, and two unstamped rows have
	// nothing to arbitrate — but a stored STAMPED row must not be overwritten
	// by a snapshot with no seq at all.
	it('writes when the incoming row has the only seq, or neither does', () => {
		expect(shouldWriteRow(row(undefined), row(7))).toBe(true);
		expect(shouldWriteRow(row(undefined), row(undefined))).toBe(true);
	});

	// The optimistic reorder path clears seq deliberately, to bypass the RAM
	// guard so the drag paints immediately (TASK-1357). Persisting that copy
	// is incidental to the intent, and letting it overwrite a stamped row
	// leaves the cache holding a row with NO seq while the cursor sits past
	// the delta that would have fixed it — unorderable by either guard on the
	// next warm boot. Codex round 4; the previous version of this file
	// asserted the opposite and enshrined the bug.
	it('REFUSES a seq-less row over a stamped one', () => {
		expect(shouldWriteRow(row(7), row(undefined))).toBe(false);
		expect(shouldWriteRow(row(0), row(undefined))).toBe(false);
	});

	// seq 0 is a real value; treating it as absent would let a stale row
	// through on the falsy check that `!existing.seq` would perform.
	it('treats seq 0 as a value, not as missing', () => {
		expect(shouldWriteRow(row(0), row(1))).toBe(true);
		expect(shouldWriteRow(row(1), row(0))).toBe(false);
	});

	// The filed race, expressed as the decision the guard has to make:
	// a retag snapshot at seq N arriving after a delta at seq N+1.
	it('refuses the BUG-2609 sequence: an un-bumped snapshot behind a landed delta', () => {
		const storedByDelta = row(101);
		const staleSnapshot = row(100);
		expect(shouldWriteRow(storedByDelta, staleSnapshot)).toBe(false);
	});
});

/**
 * shouldApplyRetag is the decision persistRetag makes per row INSIDE its
 * transaction. Unlike the transaction itself, it is reachable here.
 */
describe('shouldApplyRetag', () => {
	it('renames a row that is still in the renamed collection', () => {
		expect(shouldApplyRetag(collRow('coll-a', 'old'), 'coll-a', 'new')).toBe(true);
	});

	// The row moved between the RAM retag and this write. Applying the renamed
	// collection's slug would persist a row whose collection_id and
	// collection_slug disagree — behind the cursor, so no delta repairs it.
	it('REFUSES a row that has moved to another collection', () => {
		expect(shouldApplyRetag(collRow('coll-b', 'b-slug'), 'coll-a', 'new')).toBe(false);
	});

	it('skips a row already carrying the new slug (idempotent)', () => {
		expect(shouldApplyRetag(collRow('coll-a', 'new'), 'coll-a', 'new')).toBe(false);
	});

	// Absent means the row is not cached — or was removed by a delta. Inserting
	// it here would resurrect it behind the cursor (the BUG-2633 shape).
	it('REFUSES an absent row rather than inserting one', () => {
		expect(shouldApplyRetag(undefined, 'coll-a', 'new')).toBe(false);
	});
});

/**
 * NOT a test of persistRetag — that function is unreachable in this harness
 * (see the note at the bottom), and a no-op implementation of it would pass
 * everything in this file. Codex asked for that to be said plainly rather than
 * left for a reader to work out from the imports.
 *
 * What IS asserted is the premise that made a separate function necessary: had
 * retags kept going through persistUpserts, the seq guard would have dropped
 * them. That premise is checkable here; persistRetag's own behaviour is not.
 */
describe('the premise behind persistRetag (guard behaviour, not persistRetag itself)', () => {
	it('would refuse an un-bumped retag snapshot once a newer delta has landed', () => {
		const storedByDelta = row(200);
		// applyRetag copies the row and changes collection_slug WITHOUT
		// bumping seq — this is the value it would have persisted.
		const retagSnapshot = row(199);
		expect(shouldWriteRow(storedByDelta, retagSnapshot)).toBe(false);
	});
});

/**
 * NOT COVERED HERE, stated so nobody reads these as more than they are.
 *
 * That the guard runs INSIDE the IDB transaction — i.e. compares against what
 * is stored at write time rather than at snapshot time — is not asserted by any
 * test, because there is no IndexedDB to run it against. A plain `.test.ts`
 * file belongs to vitest's `node` project (environment: 'node', see
 * vitest.config.ts) rather than the jsdom one, which takes the
 * browser-suite glob. Node provides no `indexedDB` global, the
 * jsdom project does not implement one either, and `fake-indexeddb` is not a
 * dependency. `isSupported()` therefore returns false, so every function in
 * the persistence module that TOUCHES IDB is a no-op here — the exported
 * decision helpers tested above are pure and unaffected, which is precisely
 * why they were extracted.
 *
 * So the interleaving rests on reading the call sites (the `await store.get`
 * immediately before each `put`, both within the transaction) rather than on a
 * deterministic hook. The same gap covers persistRetag entirely: nothing here
 * exercises it, and a no-op version would pass this file.
 *
 * What DID verify the transaction actually commits is a live browser run
 * against a real server — 2625 rows persisted with the fix, and 0 rows with a
 * control build whose guard refuses every write, which also reproduced the
 * cursor-ahead-of-rows shape this bug is about. That is evidence the write
 * path works; it is not a regression test, and it will not run again on its
 * own.
 *
 * Closing the gap properly means adding `fake-indexeddb` as a dev dependency,
 * which is a project decision and cannot be done from a worktree whose
 * node_modules is a symlink — raised rather than assumed.
 */
