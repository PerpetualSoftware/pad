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
import { canOpenInViewer } from '$lib/attachments/display';

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
	/**
	 * UUID of the item whose `ItemDetail` mount should SHOW the panel.
	 *
	 * This is ROUTING, not ownership, and the difference is load-bearing:
	 * it names the host that displays the panel, and it does NOT assert that
	 * the attachment belongs to that item. The two can genuinely differ — the
	 * comment composer is reused across an item switch, so a chip sitting in
	 * an unsubmitted draft can be tapped while the pane shows a different
	 * item, and it will (correctly) route to the host in front of the user.
	 *
	 * Nothing downstream should read it as a permission or an ownership
	 * claim. Attachment authorization is the SERVER's, per attachment, against
	 * that attachment's own parent item — `handlers_storage.go` checks
	 * visibility and then edit permission on the parent it resolves itself,
	 * and the delete endpoint (`DELETE /workspaces/{ws}/attachments/{id}`) is
	 * never told which item the client thought it was acting from. What the
	 * host supplies locally (`mutationsEnabled`) decides whether to OFFER a
	 * mutation; the server decides whether to perform it.
	 *
	 * The one place the distinction leaks into UX: a panel whose "still used
	 * in this item's content" check runs against the HOST's content can only
	 * speak for that item, so it must keep the hedged wording ("may still be
	 * referenced by another item or a comment") rather than claiming the
	 * attachment is unreferenced.
	 */
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

/**
 * Image viewer (PLAN-2392 phase 3a, TASK-2428).
 *
 * Tapping an image — an inline editor image, and in phase 3c the surfaces that
 * still mount their own viewer — opens the full-screen `Lightbox`. Same problem
 * as the panel channel above, same answer: the emitters include Tiptap
 * NodeViews, which are imperative DOM and cannot mount a Svelte component, so
 * they signal through this bus and an `ItemDetail`-owned host does the mounting.
 *
 * ADDRESSING is DR-8's, unchanged and shared: a host consumes an event only
 * when BOTH `itemId` and `hostToken` are its own, because the bus is global
 * while `ItemDetail` is mounted more than once (master pane + peeked pane).
 * The token is the SAME one the panel channel uses — one token per HOST, not
 * one per channel — so `createAttachmentHostToken` is not duplicated here.
 *
 * WHY THE STRIP AND THE TIMELINE ARE NOT ON THIS CHANNEL: they mount `Lightbox`
 * directly and keep doing so. The a11y contract lives in the component, and the
 * lease stack makes coexisting mounts safe, so consolidating producers buys
 * nothing until 3c gives them a single surface to consolidate ONTO. A decision,
 * not an omission.
 *
 * `mutationsEnabled` is deliberately absent from this channel and from the
 * host: 3a's viewer has no mutating action, so it would be a dead prop. It is a
 * hard prerequisite of 3c's Delete, and when it arrives its source must be the
 * host's own gate.
 */
export interface LightboxImage {
	id: string;
	alt: string;
	/**
	 * Metadata the viewer may caption with, all NULLABLE for the same reason
	 * the panel's three are: an emitter knows only what its own surface gives
	 * it, and an inline image's HEAD probe may not have completed or may have
	 * failed, while an upload event carries only four fields
	 * (`UploadedAttachment`).
	 *
	 * `mime_type` is not decoration: it is what lets a CONSUMER re-state the
	 * DR-16 open gate over a whole set rather than trusting the one element
	 * that was clicked (TASK-2431). `width` / `height` are here ahead of any
	 * reader — phase 3b's pixel-based loading policy needs them, and adding
	 * them now costs one nullable field per producer instead of reopening the
	 * event, the host and every producer later.
	 *
	 * This is the ONLY declaration of the shape. `Lightbox.svelte` used to
	 * carry its own `{id, alt}` twin; it now re-exports this one, so the
	 * component's props and the channel's payload cannot drift.
	 */
	filename: string | null;
	mime_type: string | null;
	size_bytes: number | null;
	width: number | null;
	height: number | null;
}

export interface AttachmentViewerOpenEvent {
	/** UUID of the attachment the viewer opens ON. */
	attachmentId: string;
	/**
	 * Workspace the images are read from, CAPTURED AT EMIT — never read live
	 * from the host. The pane switches workspace without remounting, so a host
	 * that resolved the slug itself at render time could serve a viewer opened
	 * in ws1 from ws2's endpoint. The emitter knows which workspace the click
	 * happened in; that is the answer the viewer must keep.
	 */
	workspaceSlug: string;
	/**
	 * UUID of the item whose `ItemDetail` mount should SHOW the viewer.
	 *
	 * ROUTING, not ownership — see `AttachmentPanelOpenEvent.itemId` for the
	 * full argument. It names the host in front of the user; it asserts nothing
	 * about which item the attachment belongs to, and nothing about permission.
	 */
	itemId: string;
	/** Identity of the `ItemDetail` mount that owns the emitting surface. */
	hostToken: string;
	/**
	 * The set the viewer's ←/→ page through, in the emitting surface's own
	 * order. Readonly because the viewer must not reorder or mutate a set the
	 * emitter still owns.
	 */
	images: readonly LightboxImage[];
	/** Index to open at. `images[index]?.id === attachmentId` at emit. */
	index: number;
	/**
	 * The element the viewer returns focus to on close. Null when the emitter
	 * has no stable element to offer.
	 */
	invoker: HTMLElement | null;
}

/**
 * An image a producer has RESOLVED as viewable — same shape as `LightboxImage`
 * with the one field that is a GATE rather than a caption made non-nullable.
 *
 * Two types rather than one, deliberately, because the two directions have
 * genuinely different obligations. A PRODUCER must know the MIME before it
 * asks for a viewer (TASK-2433), so `notifyViewerOpen` takes this and a
 * `mime_type: null` emission is a compile error at the call site rather than a
 * silent no-op at runtime. A CONSUMER must keep accepting the nullable shape:
 * `Lightbox` is mounted directly by the strip and the timeline as well, its
 * records are live and can lose their MIME, and its own filter is what covers
 * a row that turns unsafe after the viewer is already open.
 *
 * The other three nullable fields stay nullable in both directions — phase 3b's
 * loading policy wants `width` / `height` when a producer has them and must
 * still work when it does not, and no producer has a filename for an inline
 * body image at all.
 */
export type ViewerReadyImage = Omit<LightboxImage, 'mime_type'> & { mime_type: string };

/** What `notifyViewerOpen` accepts: the event, with the set already resolved. */
export interface ViewerOpenRequest extends Omit<AttachmentViewerOpenEvent, 'images'> {
	images: readonly ViewerReadyImage[];
}

const viewerListeners = new Set<(event: AttachmentViewerOpenEvent) => void>();

/**
 * "Is this event mine?" — the single predicate every viewer host must use.
 *
 * Deliberately a separate function from the panel's rather than one generic
 * over both: the two events are different shapes, and a shared predicate would
 * have to be typed loosely enough to accept anything with two string fields.
 * The RULE is identical and stated once, in `isAddressable`: both sides must be
 * fully addressable before a comparison means anything — two empty tokens are
 * not a match, they are two absences.
 *
 * The event parameter accepts `null | undefined` where the panel's does not.
 * That is the signature TASK-2428 specifies, and it matches what both
 * functions have always DONE at runtime (`if (!event) return false`) — the
 * panel's type is simply narrower than its behaviour. Widening the panel's to
 * match is a change to a shipped surface and belongs to whoever next touches
 * it, not to this task.
 */
export function isAttachmentViewerEventForHost(
	event: AttachmentViewerOpenEvent | null | undefined,
	host: { itemId: string | null | undefined; hostToken: string | null | undefined }
): boolean {
	if (!event) return false;
	const from = { itemId: event.itemId, hostToken: event.hostToken };
	const to = { itemId: host?.itemId ?? '', hostToken: host?.hostToken ?? '' };
	if (!isAddressable(from) || !isAddressable(to)) return false;
	return from.itemId === to.itemId && from.hostToken === to.hostToken;
}

/**
 * Subscribe to open-viewer requests. Returns a dispose function — call it from
 * the host's teardown, or the listener leaks and fires into a dead component.
 * Listeners receive EVERY emission; filter with
 * `isAttachmentViewerEventForHost`.
 */
export function registerAttachmentViewerListener(
	fn: (event: AttachmentViewerOpenEvent) => void
): () => void {
	viewerListeners.add(fn);
	return () => viewerListeners.delete(fn);
}

/**
 * Request that the owning host open the image viewer.
 *
 * No-op when the event can't address a host, can't be fetched, or carries no
 * images: an emission missing any identity field would either reach nobody or
 * invite a "matches anything" reading of the predicate; one without a workspace
 * would open a viewer whose every image URL 404s (the slug is a path segment,
 * and the host deliberately does not substitute its own); and an empty set
 * would open a full-screen viewer showing nothing.
 *
 * IT ALSO POLICES THE MIME (TASK-2433). This channel used to leave that to the
 * emitter, on the reasoning that what is viewable is a judgement about the
 * surface it is emitted from. That reasoning stopped holding when TASK-2431
 * made `Lightbox` FAIL CLOSED on an unresolved MIME: a set the viewer will
 * filter to nothing does not produce an error, it produces an image that does
 * not open, with nothing thrown and nothing logged. "Resolve the MIME before
 * you emit" was then a convention, and a convention the next producer breaks
 * silently is not an invariant — so it is enforced here, where every producer
 * passes.
 *
 * THE WHOLE EMISSION IS DROPPED, not the offending entry. Filtering the set
 * would desynchronize it from `index` and `attachmentId` — the event's own
 * stated invariant is `images[index]?.id === attachmentId`, and a bus that
 * quietly renumbered a producer's set would open the viewer on a different
 * image than the one that was activated. `Lightbox`'s own `$derived` filter
 * stays as the second line: it re-applies the gate over the live records, so a
 * row that becomes unsafe AFTER the viewer opened is still dropped.
 *
 * What it deliberately does NOT police: the `index` (the viewer clamps, and
 * dropping the whole emission over an off-by-one would be a silent no-op where
 * showing the neighbouring image is harmless).
 *
 * (Named per TASK-2428's normative signature — `notifyViewerOpen`, without the
 * `Attachment` infix the sibling emitters carry. The task spells the exported
 * surface out explicitly, so it wins over the local naming rhyme.)
 */
export function notifyViewerOpen(event: ViewerOpenRequest): void {
	if (!event?.attachmentId || !event.itemId || !event.hostToken) return;
	if (!event.workspaceSlug) return;
	// `Array.isArray` rather than a truthy `length`, and an INDEXED loop rather
	// than `.some`, because this is a boundary and its input is only as good as
	// the caller. An array-like `{length: 1}` would make `.some` throw — out of
	// a notify function, into a producer's `.then`, as an unhandled rejection —
	// and a SPARSE array's holes are skipped by `.some` entirely, so `new
	// Array(1)` would sail through the gate and reach a viewer with nothing in
	// it. A type is not a runtime guarantee for a shared module.
	if (!Array.isArray(event.images) || event.images.length === 0) return;
	for (let i = 0; i < event.images.length; i++) {
		const img = event.images[i];
		// POSITIVELY allowlisted, every entry. `canOpenInViewer` answers false
		// for null and undefined alike, so "unresolved" and "resolved to
		// something we will not display" are refused by the same call — which is
		// the point: to the user they are the same non-event, and only one of
		// them was ever spelled out in a producer's comments.
		if (!img || typeof img.mime_type !== 'string') return;
		if (!canOpenInViewer(img.mime_type)) return;
	}
	for (const fn of viewerListeners) fn(event);
}
