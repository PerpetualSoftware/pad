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
