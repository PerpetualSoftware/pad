/**
 * Ordering for WHOLE-ITEM snapshots installed by the item pane (BUG-3036).
 *
 * `fieldWriteOrder.ts` orders writes to ONE field against each other. This is
 * the layer above it: every writer in the pane — a field PATCH, a title or tag
 * save, a restore, an SSE-driven re-read — answers with the whole row and
 * assigns it to `item`, so a response that is merely SLOW puts an older row on
 * screen over a newer one while the server keeps the newer.
 *
 * The order is the ROW's `seq`, not a client dispatch count. `seq` is stamped
 * from the workspace's mutation cursor on every create / update / soft-delete /
 * restore of the row, under the write lock, so of two snapshots of one row the
 * higher `seq` is the later mutation. A dispatch count would answer a different
 * question — "was a newer write SENT?" — and drop an older successful response
 * on the strength of a newer write that then FAILS, so a save the server kept
 * would never be shown (the BUG-2773 hole `fieldWriteOrder.ts`'s two counters
 * exist to avoid). `seq` also orders writers no client counter can see: other
 * tabs, agents, the SSE re-read.
 *
 * Dropping an older snapshot loses nothing that was written: field writes are
 * merged server-side (`fields_patch`), so the newer row already carries the
 * older write's value.
 */

type Snapshot = { id: string; seq?: number | null };

/**
 * True when `incoming` is a snapshot of the item currently shown and records a
 * STRICTLY earlier mutation of it than what is on screen — it must not replace
 * it.
 *
 * Deliberately narrow, so it can only ever refuse what is provably older:
 *   - a DIFFERENT item is never refused — the comparison is against the item on
 *     screen, so after a switch nothing about the previous item can block the
 *     next one's first snapshot;
 *   - EQUAL seq is accepted: re-reading an unchanged row can still carry fresher
 *     derived data (links, hydrated relation targets) that do not bump the row;
 *   - a MISSING seq on either side is accepted, which is the behaviour before
 *     this existed (a snapshot cached before the column shipped has none).
 */
export function isOlderSnapshot(current: Snapshot | null | undefined, incoming: Snapshot): boolean {
	if (!current || current.id !== incoming.id) return false;
	const have = current.seq;
	const got = incoming.seq;
	if (typeof have !== 'number' || typeof got !== 'number') return false;
	return got < have;
}
