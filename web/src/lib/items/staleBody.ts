// How a browser render says a body may be behind (BUG-3050 U3).
//
// A row whose `content_state` is `applied_pending_flush` serves a body the
// server knows is BEHIND the live collaborative document (BUG-3000): an open
// tab holds edits the row does not have yet. Every surface that renders such
// a body says so, in one of two treatments ruled on the trail:
//
// - a one-line notice above the body, where the body is the page's subject
//   (share pages, the read-only item pane, the diff's current side);
// - a muted dot with a tooltip on a MARKED row only, where the body is one
//   snippet among many (backlinks, the command palette, the conventions and
//   playbooks lists). An unmarked row renders exactly as before.
//
// The marker is a signal to re-read later. Nothing here re-fetches or
// re-sends.

/** The one wording every treatment uses. */
export const STALE_BODY_NOTICE = 'This may not include the latest edits.';

/** True when the server marked this row's body as behind edits that exist
 *  elsewhere: unflushed tab edits, or edits an editor upgrade set aside
 *  (BUG-3244). The notice and dot wording names no remedy, so it fits both;
 *  copy that DOES name one must branch on the value (isSetAside). */
export function isBodyStale(row: { content_state?: string | null } | null | undefined): boolean {
	return row?.content_state === 'applied_pending_flush' || row?.content_state === 'superseded_set_aside';
}

/** True when the row's missing edits were set aside by an editor upgrade
 *  (BUG-3244): opening the item does NOT restore them, so no copy may say it
 *  will. */
export function isSetAside(state: string | null | undefined): boolean {
	return state === 'superseded_set_aside';
}
