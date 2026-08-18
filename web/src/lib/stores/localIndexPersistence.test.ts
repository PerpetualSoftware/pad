import { describe, it, expect } from 'vitest';
import { shouldApplyRetag } from './localIndexPersistence';
import type { ItemIndexRow } from '$lib/types';

/**
 * Pure-decision coverage for the persistence layer. The write policy itself
 * (seq guard, no-seq asymmetry, tombstone gate, equal-seq merge) moved to
 * `resolveRowWrite` in `itemRowMerge.ts` (PLAN-2636 unit 2) and is pinned by
 * `itemRowMerge.test.ts`; the interleaved-in-a-real-transaction OUTCOME is
 * pinned by the `.idb.test.ts` siblings against fake-indexeddb. What remains
 * pure and persistence-owned here is `shouldApplyRetag` — the per-row decision
 * `persistRetag` makes inside its transaction.
 */

function collRow(collectionId: string, slug: string, id = 'item-1'): ItemIndexRow {
	return { id, collection_id: collectionId, collection_slug: slug } as unknown as ItemIndexRow;
}

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
	// it here would resurrect it behind the cursor (the BUG-2633 shape, now also
	// guarded durably by the tombstone store).
	it('REFUSES an absent row rather than inserting one', () => {
		expect(shouldApplyRetag(undefined, 'coll-a', 'new')).toBe(false);
	});
});

/**
 * WHAT THIS FILE DOES NOT COVER, stated so nobody reads it as more than it is.
 *
 * The transactional behaviour of every IDB-touching function — the write policy
 * running INSIDE the transaction, tombstone writes/supersession, the durable
 * retag overlay, the v1→v2 migration — is NOT exercised here. A plain
 * `.test.ts` file belongs to vitest's `node` project (environment: 'node'),
 * which has no `indexedDB`, so `isSupported()` returns false and every such
 * function is a no-op. Those outcomes are covered by the `*.idb.test.ts`
 * siblings, which run under the dedicated `idb` project against fake-indexeddb
 * (PLAN-2636 unit 1's harness). This file keeps only the pure decisions that
 * are reachable in the node env.
 */
