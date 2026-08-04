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
import { isAddressable } from '$lib/attachments/hostAddress';

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

/**
 * Attachment options panel (PLAN-2392 DR-2 / DR-8, TASK-2421).
 *
 * Tapping a file — a strip tile or an inline editor chip — opens a metadata +
 * options panel instead of downloading it. The panel is a Svelte component
 * owned by an `ItemDetail` host; the emitters include Tiptap NodeViews, which
 * are imperative DOM and cannot mount Svelte themselves. So they signal
 * through this bus, exactly as the deletion / upload channels above.
 *
 * ADDRESSING (DR-8) is the whole reason this channel carries two identity
 * fields rather than one. The bus is module-global, but `ItemDetail` is
 * mounted MORE THAN ONCE at a time — the pane host runs a master pane plus a
 * peeked pane, both showing attachment surfaces. Matching on `itemId` alone
 * is not enough (both panes can show the same item), and matching on the
 * token alone is not enough either (a host must not open a panel for an
 * attachment belonging to a different item). A host consumes an event only
 * when BOTH are its own — see `isAttachmentPanelEventForHost`.
 *
 * Permission never travels on the event: the host supplies `mutationsEnabled`
 * from its own `computeMutationsEnabled(canEdit, peeking)`. A NodeView has no
 * mutation context and must not be trusted to assert one.
 *
 * The three metadata fields are NULLABLE. A chip knows only what its options
 * give it and fills these from an asynchronous HEAD probe that may not have
 * completed, or may have failed. The panel opens immediately with whatever is
 * known and fetches the rest itself (DR-2 round 36). The strip, by contrast,
 * always populates all three from its list row.
 */
export interface AttachmentPanelOpenEvent {
	/** UUID of the attachment whose options are being opened. */
	attachmentId: string;
	/** UUID of the item the emitting surface belongs to. */
	itemId: string;
	/** Identity of the `ItemDetail` mount that owns the emitting surface. */
	hostToken: string;
	/**
	 * The element the panel positions against and returns focus to on close.
	 * Null when the emitter has no stable element to offer (the panel then
	 * falls back to its own placement / focus handling).
	 */
	anchor: HTMLElement | null;
	filename: string | null;
	mime_type: string | null;
	size_bytes: number | null;
}

/**
 * Mint the identity for ONE `ItemDetail` mount. Call it once per host and
 * pass the result to every attachment surface that host owns — the strip, the
 * body `Editor`, every `CommentEditor`. One token per host, NOT one per
 * component: surfaces of the same host must be indistinguishable to the
 * panel, while the master and peeked panes must never be.
 */
export function createAttachmentHostToken(): string {
	const c = typeof globalThis !== 'undefined' ? globalThis.crypto : undefined;
	if (c && typeof c.randomUUID === 'function') return `apanel-${c.randomUUID()}`;
	return `apanel-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
}

/**
 * "Is this event mine?" — the single predicate every panel host must use.
 *
 * Both fields must match. An empty / null token on EITHER side never matches
 * anything: a surface that was never given a token (an older call site, an
 * editor mounted outside a host) must not be able to address every host at
 * once, and a host without a token must not consume unaddressed events.
 */
export function isAttachmentPanelEventForHost(
	event: AttachmentPanelOpenEvent,
	host: { itemId: string | null | undefined; hostToken: string | null | undefined }
): boolean {
	if (!event) return false;
	// Both sides must be fully addressable before a comparison means anything:
	// two empty tokens are not a match, they are two absences. `isAddressable`
	// is the single statement of that rule (see hostAddress.ts).
	const from = { itemId: event.itemId, hostToken: event.hostToken };
	const to = { itemId: host?.itemId ?? '', hostToken: host?.hostToken ?? '' };
	if (!isAddressable(from) || !isAddressable(to)) return false;
	return from.itemId === to.itemId && from.hostToken === to.hostToken;
}

const panelListeners = new Set<(event: AttachmentPanelOpenEvent) => void>();

/**
 * Subscribe to open-panel requests. Returns a dispose function — call it from
 * the host's teardown, or the listener leaks and fires into a dead component.
 * Listeners receive EVERY emission; filter with
 * `isAttachmentPanelEventForHost`.
 */
export function registerAttachmentPanelListener(
	fn: (event: AttachmentPanelOpenEvent) => void
): () => void {
	panelListeners.add(fn);
	return () => panelListeners.delete(fn);
}

/**
 * Request that the owning host open the options panel for an attachment.
 * No-op when the event can't address a host — an emission missing any of the
 * three identity fields would either reach nobody or, worse, invite a
 * "matches anything" reading of the predicate.
 */
export function notifyAttachmentPanelOpen(event: AttachmentPanelOpenEvent): void {
	if (!event?.attachmentId || !event.itemId || !event.hostToken) return;
	for (const fn of panelListeners) fn(event);
}
