/**
 * "Which items has this tab seen archived?" (BUG-2509).
 *
 * `ItemDetail` is the one emitter of the attachment bus's parent-restore signal,
 * and deciding when to emit is subtler than the level it derives from.
 *
 * The first shape was an EDGE: latch `matched && archived`, announce on the
 * true→false transition. Two things are wrong with a per-mount latch, and they
 * pull in opposite directions:
 *
 *   - it FIRES when it shouldn't. Navigating away from an archived item also
 *     drops the level (`itemMatchesRef` goes false while `item` still holds the
 *     old row), so every such navigation announced a restore that never
 *     happened — a workspace-wide cache invalidation and a round of probes each
 *     time. Keying the latch to the item id fixes this much.
 *   - it MISSES the case the latch structurally cannot see. Archive A, navigate
 *     to B, let someone else restore A, navigate back: no archived→live edge is
 *     ever observed by this tab, nothing is announced, and a freshly built chip
 *     reads the `missing` the archived window left in the page-lifetime metadata
 *     cache and renders dead — the original bug, by another route.
 *
 * Both dissolve if the memory is keyed by ITEM and outlives the mount, which is
 * what this is: mark an item when it is seen archived, and when it is next seen
 * LIVE, that is a restore this tab has to reconcile — whether it happened in
 * front of the user or while they were looking at something else.
 *
 * Module-level, like the event bus it feeds, and for the same reason: the pane
 * mounts `ItemDetail` more than once at a time and remounts it across
 * navigation, so per-instance state cannot carry this. `consumeRestored` clears
 * as it reports, so two mounts showing the same restored item announce once
 * between them rather than each — the announce is idempotent, but a second
 * workspace-wide invalidation is pure waste.
 *
 * Ids are UUIDs (globally unique), so the set needs no workspace qualifier; the
 * workspace is carried on the announce itself, from the emitting host.
 */

/**
 * Unbounded for the page's lifetime, deliberately (raised in review; declined
 * with reason). It holds one uuid string per item this tab has SEEN archived and
 * not yet seen live again — bounded in practice by how many archived items a
 * person opens in one session. Eviction is the risky option, not the safe one:
 * dropping a mark early reintroduces exactly the bug this exists to fix, silently
 * and only for users whose session ran long enough. A leak measured in bytes is
 * the better trade against a correctness regression measured in dead attachments.
 */
const seenArchived = new Set<string>();

/** Remember that `itemId` is currently archived. Idempotent. */
export function markItemArchived(itemId: string): void {
	if (!itemId) return;
	seenArchived.add(itemId);
}

/**
 * Report whether `itemId` was seen archived by this tab, clearing the mark so
 * only ONE caller observes each restore. Call when the item is known LIVE.
 */
export function consumeItemRestored(itemId: string): boolean {
	if (!itemId) return false;
	return seenArchived.delete(itemId);
}

/**
 * Decide what a host should do for the item it is currently showing.
 *
 * Split out from the component because `ItemDetail` resists unit mounting (its
 * module graph reaches `localStorage`, which jsdom does not usefully provide),
 * and the away-and-back sequence is otherwise unpinnable.
 *
 * `matched` is the host's own "the loaded item is the one the route asked for"
 * predicate. Without it, a host mid-switch — still holding item A's row while
 * the route already names B — would mark or announce for the wrong item.
 */
export function reconcileArchivedState(args: {
	matched: boolean;
	archived: boolean;
	itemId: string;
}): { restoredItemId: string | null } {
	const { matched, archived, itemId } = args;
	if (!matched || !itemId) return { restoredItemId: null };
	if (archived) {
		markItemArchived(itemId);
		return { restoredItemId: null };
	}
	return { restoredItemId: consumeItemRestored(itemId) ? itemId : null };
}

/** Test-only: drop all marks so cases cannot leak between specs. */
export function __resetArchivedItemRegistry(): void {
	seenArchived.clear();
}
