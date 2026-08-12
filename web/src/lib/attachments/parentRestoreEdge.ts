/**
 * The archive → restore EDGE, as a pure function (BUG-2509).
 *
 * `ItemDetail` is the one emitter of the attachment bus's parent-restore signal,
 * and deciding when to emit is subtler than the level it is derived from. The
 * obvious form — latch `itemMatchesRef && isArchived` and emit on true→false —
 * is WRONG, and wrong in a way that is invisible in the component: navigating
 * AWAY from an archived item also flips that expression false (`itemMatchesRef`
 * goes false while `item` still holds the old row), so every such navigation
 * announced a restore that never happened. The announce invalidates a
 * workspace's metadata cache and prompts probes, so a spurious one is real
 * waste, and it races the actual item transition.
 *
 * A restore is therefore not "the level went false" but "the SAME item, still
 * the loaded and matched one, went from archived to live". That needs the id in
 * the latch, not just the boolean.
 *
 * It lives here, as a function over plain values, because the component itself
 * resists unit mounting (its module graph reaches `localStorage`, which jsdom
 * does not usefully provide) — so the only way to pin the switch-away case is to
 * make the decision testable on its own.
 */

export interface ParentRestoreEdgeInput {
	/** Id of the item last seen archived AND matched; '' when there was none. */
	prevArchivedItemId: string;
	/** Does the loaded item correspond to the route's ref right now? */
	matched: boolean;
	/** Is the loaded item archived (soft-deleted) right now? */
	archived: boolean;
	/** Id of the loaded item, '' when there is none. */
	itemId: string;
}

export interface ParentRestoreEdgeResult {
	/** The latch value to carry forward. */
	nextArchivedItemId: string;
	/** The item to announce a restore for, or null for "no edge". */
	restoredItemId: string | null;
}

export function parentRestoreEdge(input: ParentRestoreEdgeInput): ParentRestoreEdgeResult {
	const { prevArchivedItemId, matched, archived, itemId } = input;
	const nextArchivedItemId = matched && archived && itemId ? itemId : '';
	if (nextArchivedItemId === prevArchivedItemId) {
		return { nextArchivedItemId, restoredItemId: null };
	}
	// Only a transition OUT of "this id was archived" can be a restore, and only
	// when that same id is still what we are showing, still matched, and now
	// live. A switch away leaves `matched` false or `itemId` different — both
	// are "we stopped looking at an archived item", not "it came back".
	const isRestore =
		prevArchivedItemId !== '' &&
		!archived &&
		matched &&
		itemId === prevArchivedItemId;
	return { nextArchivedItemId, restoredItemId: isRestore ? itemId : null };
}
