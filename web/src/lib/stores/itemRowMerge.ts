// itemRowMerge — pure row-merge + write-decision logic shared by the in-RAM
// localIndex store and the IDB persistence layer (PLAN-2636 unit 2).
//
// WHY A SHARED MODULE. The persisted localIndex cache and the in-RAM store had
// drifted into three separate decisions about whether an incoming row may
// overwrite a stored one: RAM's `mergeRow`, the persistence layer's boolean
// `shouldWriteRow`, and — nowhere — any memory of deletes or out-of-band
// fields. BUG-2633 / BUG-2634 / BUG-2635 are that one gap seen three ways. The
// fix is a single pure decision function (`resolveRowWrite`) plus the two
// projection-merge helpers RAM already had, lifted here so both layers import
// one implementation. The persistence layer must NOT import the Svelte store
// (reactivity + import cycle); these helpers are pure, so this is a move.
//
// `preserveProjectionMetadata` and `mergeEqualSeqProjection` are moved VERBATIM
// from `localIndex.svelte.ts` — RAM behaviour must be bit-identical after the
// move, pinned by the existing localIndex tests.

import type { ItemIndexRow } from '$lib/types';

/**
 * A hard-delete tombstone: the id that was removed and the `seq` (cursor) the
 * removal was durable at. A tombstone IS ordering evidence — a delayed snapshot
 * for the same id at an equal-or-older seq must not resurrect the row
 * (BUG-2633). Persisted in its own IDB object store; see localIndexPersistence.
 */
export interface Tombstone {
	id: string;
	deletedAtSeq: number;
}

/**
 * The outcome of `resolveRowWrite`: either write `row` (which may be a merged
 * projection, not the raw incoming row) or skip the write entirely.
 */
export type RowWriteDecision =
	| { action: 'write'; row: ItemIndexRow }
	| { action: 'skip' };

/**
 * Full mutation responses intentionally omit local-first-only projections
 * (e.g. `is_unparented`). Carry the existing value across optimistic
 * replacements until the authoritative index/delta row for that seq arrives.
 *
 * Moved verbatim from localIndex.svelte.ts (PLAN-2636 unit 2).
 */
export function preserveProjectionMetadata(
	existing: ItemIndexRow | undefined,
	next: ItemIndexRow,
): ItemIndexRow {
	if (existing && !('is_unparented' in next) && 'is_unparented' in existing) {
		return { ...next, is_unparented: existing.is_unparented };
	}
	return next;
}

/**
 * An index/delta row with the SAME seq as an optimistic mutation response is
 * not stale when it contributes projection metadata that response omitted.
 * Returns the merged row when the incoming row adds/changes a projection bit,
 * or null when it contributes nothing (so the caller skips the write).
 *
 * Moved verbatim from localIndex.svelte.ts (PLAN-2636 unit 2).
 */
export function mergeEqualSeqProjection(
	existing: ItemIndexRow,
	incoming: ItemIndexRow,
): ItemIndexRow | null {
	if ('is_unparented' in incoming && incoming.is_unparented !== existing.is_unparented) {
		return { ...existing, is_unparented: incoming.is_unparented };
	}
	return null;
}

/**
 * Decide whether `incoming` may overwrite the persisted row for its id, given
 * the currently stored row and any tombstone. This is the persistence layer's
 * write policy for `persistUpserts` / `persistDelta` — the single function that
 * replaces the boolean `shouldWriteRow`, now aware of deletes (BUG-2633) and
 * equal-seq projection merges (BUG-2635).
 *
 * Check order (the contract on the PLAN-2636 trail):
 *
 *   1. TOMBSTONE (BUG-2633). A tombstone is ordering evidence, so the same
 *      no-seq asymmetry a stored row gets applies to it: a seq-less incoming,
 *      or one at seq <= the tombstone's, is a stale resurrection → skip. A
 *      strictly-newer incoming supersedes the delete → fall through to write;
 *      the caller deletes the tombstone in the SAME transaction.
 *
 *   2. SEQ (unchanged from BUG-2609, including the no-seq asymmetry): a stored
 *      stamped row is never overwritten by a strictly-older incoming, nor by a
 *      seq-less one (the optimistic-reorder path clears seq to bypass the RAM
 *      guard and paint immediately — persisting it over an authoritative row
 *      would strand the cache with an unorderable row while the cursor sits
 *      past the correcting delta; see shouldWriteRow's original note).
 *
 *   3. EQUAL SEQ (BUG-2635): MERGE, not blind-accept. This is exactly RAM's
 *      `mergeRow` equal-seq branch, now cross-tab-safe — a second tab that
 *      lacks the row in RAM can no longer persist an unmerged snapshot over
 *      another tab's merged row.
 *
 *   4. ON WRITE: `preserveProjectionMetadata` first, mirroring RAM. This also
 *      closes the unfiled cross-tab sibling of 2635 — a tab persisting a
 *      mutation-response snapshot that omits `is_unparented` no longer drops
 *      the projection field from IDB on a newer-seq write.
 *
 * Pure: no IDB, no store, no side effects. The caller owns the transaction and
 * the tombstone delete.
 */
export function resolveRowWrite(
	stored: ItemIndexRow | undefined,
	tombstone: Tombstone | undefined,
	incoming: ItemIndexRow,
): RowWriteDecision {
	// 1. Tombstone check (BUG-2633). Same no-seq asymmetry as a stored row: a
	//    tombstone carries ordering evidence the incoming seq-less snapshot
	//    lacks, so it cannot resurrect the row.
	if (tombstone) {
		if (incoming.seq === undefined || incoming.seq <= tombstone.deletedAtSeq) {
			return { action: 'skip' };
		}
		// incoming.seq > tombstone.deletedAtSeq — authoritative supersession.
		// A tombstoned id has no stored row (the delete removed it), so the seq
		// block below is a no-op and we fall through to the write.
	}

	// 2 & 3. Seq guard, mirroring shouldWriteRow but merging (not accepting) at
	//        equal seq.
	if (stored && stored.seq !== undefined) {
		// Stored row is stamped and the incoming one is not: no claim to be newer.
		if (incoming.seq === undefined) return { action: 'skip' };
		if (incoming.seq < stored.seq) return { action: 'skip' };
		if (incoming.seq === stored.seq) {
			const merged = mergeEqualSeqProjection(stored, incoming);
			if (!merged) return { action: 'skip' };
			return { action: 'write', row: merged };
		}
		// incoming.seq > stored.seq — newer; fall through to the write.
	}

	// 4. Preserve projection metadata on write (mirrors RAM's mergeRow, which
	//    runs preserve before the seq compare; at equal seq the branch above
	//    already handled the merge, so this covers the fresh-insert / stored-
	//    seq-less / strictly-newer paths).
	return { action: 'write', row: preserveProjectionMetadata(stored, incoming) };
}
