/**
 * App-wide attachment deletion bus (PLAN-2382 / TASK-2384).
 *
 * An attachment can be deleted from more than one surface — the item detail
 * attachment strip, Settings → Storage — while other surfaces are mounted and
 * holding it: editor NodeViews, another pane's strip. None of them find out on
 * their own. An `<img>` that already painted never re-requests, and a file
 * chip's link makes no request until it's clicked, so without a broadcast both
 * keep presenting a row the server no longer has until the next reload.
 *
 * Deliberately module-level and framework-agnostic: subscribers are Tiptap
 * NodeViews (imperative DOM) and Svelte components alike. It lives here rather
 * than under `components/editor/` because it is attachment-domain state, not an
 * editor concern — the strip both emits and consumes it and never touches
 * Tiptap (Codex round 18).
 *
 * Scope: this process only. It does NOT cover another user's deletes or
 * another browser tab — surfaces still need their own reconciliation for that
 * (the strip treats a 404 on delete as authoritative for exactly that reason).
 */

const listeners = new Set<(uuid: string) => void>();

/**
 * Subscribe to deletions. Returns a dispose function — call it from the
 * component's teardown / the NodeView's destroy(), or the listener leaks and
 * fires into a dead view.
 */
export function registerAttachmentDeletionListener(fn: (uuid: string) => void): () => void {
	listeners.add(fn);
	return () => listeners.delete(fn);
}

/**
 * Announce that `uuid` is gone. Call only after the server confirms the
 * delete — subscribers treat it as authoritative and latch it.
 */
export function notifyAttachmentDeleted(uuid: string): void {
	if (!uuid) return;
	for (const fn of listeners) fn(uuid);
}
