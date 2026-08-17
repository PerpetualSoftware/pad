// ItemDetail collection-snapshot adoption decision (BUG-2602).
//
// ItemDetail has seven sites that assign the `collection` snapshot, racing
// under a generation fence (`collectionGen`) that orders STARTS — and one
// site (loadData's cross-collection escape hatch) that deliberately admitted
// a generation-stale write when it fetched a different collection. That
// disjunct made the fences compose wrongly: a loadData continuation spanning
// a cross-collection move could restore the SOURCE collection over the
// freshly adopted TARGET (the live item, itemGen-fenced, keeps the move —
// so the pane rendered item and collection from different collections).
//
// This helper is the single pure decision every write routes through. The
// invariant it enforces is SEMANTIC, not ordinal: the pane renders the LIVE
// item's collection. `collection_id` is the anchor — stable across renames
// (where slugs lie) and changed by moves (where slugs can be reused).
//
// Decision:
//   1. Semantic veto — if a live item is loaded and carries a collection id,
//      a snapshot for any OTHER collection is wrong NOW, regardless of how
//      fresh its fetch was. This also closes a latent reused-slug hazard:
//      an SSE-refresh fetch by slug can return a FOREIGN collection when
//      the slug was re-owned after a rename.
//   2. Generation order — among semantically-valid snapshots of the SAME
//      collection (schema edits, refreshes), newest-started wins, exactly
//      what `collectionGen` always meant.
//   3. Cross-collection correction — a semantically-valid snapshot for a
//      DIFFERENT collection than currently shown is admitted even when
//      generation-stale: this is the legitimate half of the old escape
//      hatch (a stale refresh for the old collection must not block
//      correcting to the live item's collection).
export interface AdoptCollectionInput {
	/** The fetched snapshot's collection id. */
	fetchedId: string;
	/** The LIVE item's collection_id at write time (null → no item loaded / not carried). */
	liveItemCollectionId: string | null;
	/** The currently shown collection's id (null → none loaded yet). */
	currentCollectionId: string | null;
	/** Whether the writer's captured generation is still the latest. */
	genFresh: boolean;
}

export function shouldAdoptCollection(input: AdoptCollectionInput): boolean {
	const { fetchedId, liveItemCollectionId, currentCollectionId, genFresh } = input;
	// 1. Semantic veto: disagreeing with the live item is wrong regardless
	//    of freshness.
	if (liveItemCollectionId !== null && liveItemCollectionId !== fetchedId) return false;
	// 2. Same-collection refreshes: newest-started wins.
	if (currentCollectionId === fetchedId) return genFresh;
	// 3. Cross-collection correction toward the live item's collection (or
	//    first load with nothing to disagree with): admit. When no item is
	//    loaded, generation order is the only signal left.
	if (liveItemCollectionId === null) return genFresh;
	return true;
}
