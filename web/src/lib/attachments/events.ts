/**
 * App-wide attachment event bus (PLAN-2382).
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
 * Scope: this process only. It does NOT cover another user's changes or
 * another browser tab — surfaces still need their own reconciliation for that
 * (the strip treats a 404 on delete as authoritative for exactly that reason).
 */

import { invalidateAttachmentMetadata } from '$lib/components/editor/attachment-metadata';
import type { AttachmentUploadResult } from '$lib/types';

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

/**
 * The full "this attachment is gone" reconciliation: tell the live views AND
 * drop the cached HEAD metadata, so a surface that re-resolves the reference
 * later doesn't get a hit describing a deleted row.
 *
 * Every delete surface needs both, and a 404 is just as authoritative as a
 * 204 — four call sites were repeating the pair, which is one omission away
 * from a surface that silently stops propagating. Prefer this over calling
 * the two halves separately.
 *
 * (Imports the metadata cache from components/editor: the cache predates this
 * module and moving it is a bigger change than this cleanup warrants.)
 */
export function announceAttachmentDeleted(workspaceSlug: string, uuid: string): void {
	if (!uuid) return;
	notifyAttachmentDeleted(uuid);
	invalidateAttachmentMetadata(workspaceSlug, uuid);
}

/**
 * Uploads (TASK-2385).
 *
 * The editor's paste / drag-drop upload plugin is the only thing that knows a
 * file just landed, and nothing above it is watching — so an attachment
 * dropped into the body wouldn't appear in the item attachment strip until the
 * next load. Rather than thread a callback down through two <Editor> branches,
 * the upload closure announces here and the strip picks it up, mirroring the
 * deletion direction above.
 *
 * `itemId` is REQUIRED and is the association the server actually persisted:
 * an upload made without item context leaves attachments.item_id NULL, so
 * showing an optimistic tile for it would be a lie that vanishes on refresh.
 * Emitters must skip those rather than pass a placeholder.
 */
export interface UploadedAttachment {
	id: string;
	filename: string;
	mime_type: string;
	size_bytes: number;
}

/**
 * Narrow an upload response to what subscribers need. Both upload paths (body
 * editor, comment composer) were hand-mapping the same four fields, which is
 * how the two drift apart.
 */
export function toUploadedAttachment(result: AttachmentUploadResult): UploadedAttachment {
	return {
		id: result.id,
		filename: result.filename,
		mime_type: result.mime,
		size_bytes: result.size,
	};
}

const uploadListeners = new Set<(itemId: string, attachment: UploadedAttachment) => void>();

export function registerAttachmentUploadListener(
	fn: (itemId: string, attachment: UploadedAttachment) => void
): () => void {
	uploadListeners.add(fn);
	return () => uploadListeners.delete(fn);
}

/** Announce a persisted upload. No-op without an item association. */
export function notifyAttachmentUploaded(
	itemId: string | null | undefined,
	attachment: UploadedAttachment
): void {
	if (!itemId || !attachment?.id) return;
	for (const fn of uploadListeners) fn(itemId, attachment);
}
