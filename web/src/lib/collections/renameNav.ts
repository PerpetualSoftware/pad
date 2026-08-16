// Cross-tab collection-rename re-navigation decision (BUG-2272).
//
// The collection route (`[collection]/+page.svelte`) subscribes to
// `collection_updated` SSE events. On a RENAME, the route's URL slug is now
// dead and must be re-targeted to the new slug. This helper is the pure
// decision at the heart of that handler — extracted here because the live
// handler lives inside a component `$effect` with no mount seam, so this is the
// only way to deterministically unit-test the two subtle behaviours it
// balances:
//
//   1. Reject a rename computed against a STALE collection snapshot from a
//      PREVIOUS route (the reused-slug X/B → Y/A window). `routeSlug`
//      (page.params.collection) flips synchronously on navigation, but the
//      loaded `collection` snapshot is refreshed asynchronously, so the
//      handler's stable-id gate can still pass for the previous collection —
//      firing a goto that hijacks the new navigation.
//
//   2. STILL apply a legitimate chained-rename CONTINUATION on the current
//      route during the goto→reload window (a live A→B commits, then B→C
//      arrives while `loadCollection(B)` is in flight so the snapshot is briefly
//      stale). Dropping it would strand the route on the dead intermediate slug.
//
// The distinguishing signal is `renameNav` — the synchronous tracker of the
// slug we most recently goto'd. It equals `routeSlug` ONLY in the continuation
// window (we just navigated there and the route caught up); it is reset to
// null / a different value on a real cross-collection navigation, so it never
// equals the reused slug in case 1.

export interface RenameNavInput {
	/** The event's OLD (routed-by) slug — `event.collection` on a `collection_updated`. */
	eventOldSlug: string;
	/** The event's NEW slug — `event.new_slug` (present only on a rename). */
	eventNewSlug: string;
	/**
	 * The loaded collection snapshot's slug. May be STALE — briefly the previous
	 * collection's slug during a cross-collection load, or the pre-rename slug
	 * during a same-collection goto→reload window.
	 */
	loadedCollectionSlug: string;
	/** The live route slug (`page.params.collection`). */
	routeSlug: string;
	/** The synchronous pending-rename tracker (the slug we last goto'd), or null. */
	renameNav: string | null;
}

/**
 * Decide the slug a collection-rename event should navigate the route to, or
 * `null` to skip (already there, superseded/duplicate replay, or a stale
 * foreign snapshot). The caller (the `+page.svelte` handler) sets
 * `renameNav = <result>` and `goto`s it when non-null.
 */
export function resolveRenameNavTarget(input: RenameNavInput): string | null {
	const { eventOldSlug, eventNewSlug, loadedCollectionSlug, routeSlug, renameNav } = input;

	// A pending-rename CONTINUATION on THIS route: we just goto'd `renameNav`,
	// the route caught up (`renameNav === routeSlug`), and this event renames the
	// slug we're now on. Admit it even though the loaded snapshot is briefly
	// stale. Never holds in the reused-slug X/B → Y/A case (renameNav is
	// reset / ≠ the new route slug on a real cross-collection navigation).
	const pendingContinuation = renameNav === routeSlug && eventOldSlug === routeSlug;

	// Reject a snapshot that belongs to the PREVIOUS collection (the reused-slug
	// hijack window) unless it's the continuation above.
	if (loadedCollectionSlug !== routeSlug && !pendingContinuation) return null;

	// Chained/replayed serialization: retarget from wherever we're actually
	// headed (`renameNav` if a rename is pending, else the live route slug), so a
	// burst A→B→C converges on the final slug and a stale/duplicate replay of an
	// already-applied rename is dropped.
	const believed = renameNav ?? routeSlug;
	if (eventOldSlug === believed && eventNewSlug !== believed) return eventNewSlug;
	return null;
}

// --- Sync-side rename reconciliation (BUG-2601) ---
//
// The SSE handler above is the PRIMARY rename recovery, but it only fires if
// the `collection_updated` event actually arrives. A client that missed it
// (replay-buffer gap, disconnect) is stranded: delta-sync reconciles ITEM
// changes only, `/changes` says nothing about collection renames — a
// rename-only gap even reports `caught_up` — and every slug-keyed fetch on
// the route 404s until a manual navigation. This helper is the pure decision
// for the sync-pass fallback: given the freshly fetched collections list,
// heal the route iff its slug is DEAD and the loaded collection's STABLE id
// maps to a live entry under a new slug.

export interface SyncRenameInput {
	/** The mounted collection snapshot's stable id (null → nothing loaded, skip). */
	collectionId: string | null;
	/** The live route slug (`page.params.collection`). */
	routeSlug: string;
	/** The freshly fetched workspace collections (only id + slug are read). */
	collections: Array<{ id: string; slug: string }>;
	/** The synchronous pending-rename tracker (slug we last goto'd), or null. */
	renameNav: string | null;
}

/**
 * Decide the slug a sync pass should heal the route to, or `null` to skip.
 *
 * Deliberately conservative — it only acts when every one of these holds:
 *
 *   - Something is loaded (`collectionId` non-null): a cold route with no
 *     snapshot has no stable id to reconcile by; nothing to do client-side.
 *   - No rename goto is already in flight (`renameNav` null or caught up to
 *     the route): the SSE / reorder-404 paths own their own navigation, and
 *     dueling gotos must not race.
 *   - The route slug is ABSENT from the live list: a present slug is either
 *     our own collection (nothing to heal) or a REUSED slug now naming a
 *     different collection — ambiguous, and yanking the user off a live
 *     route is worse than leaving the SSE path to sort it out.
 *   - The stable id maps to a live entry with a different slug: a missing id
 *     means DELETED, not renamed — the not-found flows own that.
 */
export function resolveSyncRenameTarget(input: SyncRenameInput): string | null {
	const { collectionId, routeSlug, collections, renameNav } = input;
	if (!collectionId) return null;
	if (renameNav !== null && renameNav !== routeSlug) return null;
	if (collections.some((c) => c.slug === routeSlug)) return null;
	const fresh = collections.find((c) => c.id === collectionId);
	if (!fresh || fresh.slug === routeSlug) return null;
	return fresh.slug;
}
