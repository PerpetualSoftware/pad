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
	/**
	 * Pixel dimensions, when the server returned them (nullable — a non-image, or
	 * an image whose dimensions it couldn't read). Carried so a freshly uploaded
	 * image opened in the viewer can classify for the DR-5b loading policy
	 * (TASK-2459) instead of falling to `unknown` and pulling the original
	 * outright; the upload response has them, this narrowing used to DROP them.
	 */
	width: number | null;
	height: number | null;
}

/**
 * Narrow an upload response to what subscribers need. Both upload paths (body
 * editor, comment composer) were hand-mapping the same fields, which is how the
 * two drift apart.
 */
export function toUploadedAttachment(result: AttachmentUploadResult): UploadedAttachment {
	return {
		id: result.id,
		filename: result.filename,
		mime_type: result.mime,
		size_bytes: result.size,
		width: result.width ?? null,
		height: result.height ?? null,
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
 * `mutationsEnabled` is on the HOST but deliberately NOT on this CHANNEL (since
 * 3c-i's Delete, TASK-2474): it is a LIVE permission (the host's `canEdit &&
 * !peeking`), read on the far side of an async confirmation, not a value an
 * emitter could snapshot at open — so `AttachmentViewerHost` takes it (and the
 * delete-warning content getters) as PROPS and forwards them to `Lightbox`,
 * while the open EVENT stays permission-free.
 */
export interface LightboxImage {
	id: string;
	alt: string;
	/**
	 * Metadata the viewer may caption with, all NULLABLE for the same reason
	 * the panel's three are: an emitter knows only what its own surface gives
	 * it, and an inline image's HEAD probe may not have completed or may have
	 * failed, while an upload event carries only the `UploadedAttachment` fields
	 * (which now include the pixel dimensions, threaded for the DR-5b policy —
	 * TASK-2459).
	 *
	 * `mime_type` is not decoration: it is what lets a CONSUMER re-state the
	 * DR-16 open gate over a whole set rather than trusting the one element
	 * that was clicked (TASK-2431). `width` / `height` are here ahead of any
	 * reader — phase 3b's pixel-based loading policy needs them, and adding
	 * them now costs one nullable field per producer instead of reopening the
	 * event, the host and every producer later.
	 *
	 * This is the ONLY declaration of the shape. `Lightbox.svelte` used to
	 * carry its own `{id, alt}` twin; it now imports this one, so the
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

/**
 * Unified attachment surface (PLAN-2392 phase 3c-ii, TASK-2485).
 *
 * 3c-i left two ways to open an attachment: the options PANEL (a file, via the
 * panel channel above) and the image VIEWER (an image, via the viewer channel).
 * 3c-ii converges them onto ONE surface — a grown `Lightbox` that opens ANY
 * attachment, image or file or a row whose type is not yet resolved — and this
 * is its channel. Same bus discipline as its two siblings, and the SAME host
 * token (one per `ItemDetail` mount, not one per channel), so a host addresses
 * all three with the identity it already has.
 *
 * ADDITIVE FOR NOW. T1 only introduces the channel; no producer emits on it and
 * no host consumes it yet (T2a builds the host, T4a repoints the producers, T4b
 * deletes the two legacy channels and `ViewerReadyImage`). The panel and viewer
 * channels above keep working untouched until then.
 *
 * TWO THINGS IT DELIBERATELY DOES DIFFERENTLY FROM THE VIEWER CHANNEL:
 *
 *  1. NO ADMISSION MIME GATE. `notifyViewerOpen` fails the whole emission closed
 *     on an unresolved or non-allowlisted MIME, because that channel only ever
 *     opens IMAGES and an unviewable image is a silent no-op. The converged
 *     surface opens files and unresolved rows too, so admission cannot depend on
 *     the MIME: the allowlist governs the SLOT ARM instead (`getSurfaceRenderer`
 *     picks the raster viewer vs the icon / download fallback), never whether
 *     the surface opens. A null `mime_type` is data here, not a reason to drop —
 *     the old gate's per-emission drop becomes per-entry pass-through.
 *
 *  2. NO `anchor`. The panel positioned itself against an anchor element; the
 *     converged surface is centered (role=dialog on both breakpoints, AM-4), so
 *     the only element it needs is the focus-return target — `invoker`, exactly
 *     as the viewer channel already carries.
 *
 * IT SNAPSHOTS THE SET AT EMISSION. The two legacy channels pass the caller's
 * event and array straight through (`notifyViewerOpen`, above). This one is the
 * convergence point every producer funnels through, and some hand over a set
 * they still hold and mutate (a strip re-sorting, a timeline re-deriving). A
 * caller that mutates its array — or a record inside it — after calling must not
 * be able to reach into an open surface, so the array AND each record are copied
 * here. A shallow array copy is not enough: the records stay shared references
 * until each is spread.
 */
export interface AttachmentSurfaceOpenEvent {
	/** UUID of the attachment the surface opens ON. */
	attachmentId: string;
	/**
	 * Workspace the attachments are read from, CAPTURED AT EMIT — never read live
	 * from the host, exactly as the viewer channel captures it and for the same
	 * reason: the pane switches workspace without remounting, so a host that
	 * resolved the slug at render time could serve a surface opened in ws1 from
	 * ws2's endpoint. REQUIRED — there is no host fallback.
	 */
	workspaceSlug: string;
	/**
	 * UUID of the item whose `ItemDetail` mount should SHOW the surface. ROUTING,
	 * not ownership — see `AttachmentPanelOpenEvent.itemId` for the full argument.
	 * It names the host in front of the user; it asserts nothing about which item
	 * the attachment belongs to, and nothing about permission.
	 */
	itemId: string;
	/** Identity of the `ItemDetail` mount that owns the emitting surface. */
	hostToken: string;
	/**
	 * The set the surface pages through, in the emitting surface's own order.
	 * MIME-NULLABLE (`LightboxImage`, not the viewer channel's resolved
	 * `ViewerReadyImage`): a file or an unresolved row is a first-class member
	 * here, so the type must admit a null `mime_type`. Readonly because the
	 * surface must not reorder or mutate a set the emitter still owns — and copied
	 * at emit regardless (see `notifyAttachmentSurfaceOpen`).
	 */
	images: readonly LightboxImage[];
	/** Index to open at. `images[index]?.id === attachmentId` at emit. */
	index: number;
	/**
	 * The element the surface returns focus to on close. Null when the emitter
	 * has no stable element to offer.
	 */
	invoker: HTMLElement | null;
	/**
	 * Single-attachment SEEDS — the metadata a producer already knows for the
	 * opened attachment, all NULLABLE (a chip's HEAD probe may be incomplete or
	 * absent, and an inline body image has no filename at all). When `images` is
	 * present these DESCRIBE `images[index]`, and `notifyAttachmentSurfaceOpen`
	 * rejects an emission where a non-null seed disagrees with that record: a seed
	 * that contradicts the set it indexes is a producer bug, not two captions to
	 * reconcile downstream.
	 */
	filename: string | null;
	mime_type: string | null;
	size_bytes: number | null;
}

/**
 * "Is this event mine?" — the single predicate every surface host must use.
 *
 * The RULE is DR-8's, identical to the panel's and the viewer's and stated once
 * in `isAddressable`: both sides fully addressable, both fields equal. A separate
 * function per channel rather than one generic over all three — the events are
 * different shapes, and a shared predicate would have to be typed loosely enough
 * to accept anything with two string fields. Stating the rule a third time is
 * the price of one family a future reader can see at a glance. Accepts a
 * null / undefined event, as the viewer's does.
 */
export function isAttachmentSurfaceEventForHost(
	event: AttachmentSurfaceOpenEvent | null | undefined,
	host: { itemId: string | null | undefined; hostToken: string | null | undefined }
): boolean {
	if (!event) return false;
	const from = { itemId: event.itemId, hostToken: event.hostToken };
	const to = { itemId: host?.itemId ?? '', hostToken: host?.hostToken ?? '' };
	if (!isAddressable(from) || !isAddressable(to)) return false;
	return from.itemId === to.itemId && from.hostToken === to.hostToken;
}

const surfaceListeners = new Set<(event: AttachmentSurfaceOpenEvent) => void>();

/**
 * Subscribe to open-surface requests. Returns a dispose function — call it from
 * the host's teardown, or the listener leaks and fires into a dead component.
 * Listeners receive EVERY emission; filter with `isAttachmentSurfaceEventForHost`.
 */
export function registerAttachmentSurfaceListener(
	fn: (event: AttachmentSurfaceOpenEvent) => void
): () => void {
	surfaceListeners.add(fn);
	return () => surfaceListeners.delete(fn);
}

const isStringOrNull = (v: unknown): v is string | null => v === null || typeof v === 'string';
const isNumberOrNull = (v: unknown): v is number | null => v === null || typeof v === 'number';

/**
 * Request that the owning host open the unified attachment surface.
 *
 * The identity / workspace / non-empty-set guards are the viewer channel's,
 * unchanged: an emission missing an identity field reaches nobody or invites a
 * "matches anything" reading of the predicate; one missing the workspace opens a
 * surface whose every URL 404s (the slug is a path segment, and the host does
 * not substitute its own); an empty set opens a surface showing nothing. The
 * `Array.isArray` + indexed-loop posture is the viewer channel's too — an
 * array-like or a sparse array is caller input at a boundary, not a runtime
 * guarantee a type can make.
 *
 * WHAT DIFFERS from `notifyViewerOpen`:
 *
 *  - NO MIME gate. Files and unresolved rows are the point of the converged
 *    surface, so a null / non-allowlisted `mime_type` passes through rather than
 *    dropping the emission. The renderer arm decides what to show; admission does
 *    not.
 *  - THREE CONSISTENCY REJECTIONS that enforce the event's own invariants at the
 *    boundary, where the old channel only documented them: the index must be in
 *    range; `images[index]` must be the attachment the event opens on; and any
 *    non-null flat seed must agree with that record.
 *  - A DEEP SNAPSHOT of the set: a NEW array of NEW records built by explicit
 *    field projection, so a caller mutating its set after the call — and any
 *    prototype trickery or stray property on its records — cannot reach an
 *    already-open surface.
 */
export function notifyAttachmentSurfaceOpen(event: AttachmentSurfaceOpenEvent): void {
	if (!event || typeof event !== 'object') return;
	// Read every scalar the emission needs EXACTLY ONCE, up front. A getter or
	// proxy that answered one way for a guard and another for the snapshot cannot
	// split the two if there is only one read. The producers are all first-party,
	// but this is the shared boundary every one of them funnels through, and "a
	// type is not a runtime guarantee for a shared module" is this file's own rule
	// (see `notifyViewerOpen`).
	const attachmentId = event.attachmentId;
	const workspaceSlug = event.workspaceSlug;
	const itemId = event.itemId;
	const hostToken = event.hostToken;
	const index = event.index;
	const invoker = event.invoker;
	const seedFilename = event.filename;
	const seedMime = event.mime_type;
	const seedSize = event.size_bytes;
	const rawImages = event.images;

	// Require the four identity/workspace fields to be non-empty STRINGS, not just
	// truthy — the same primitive-or-null discipline the records get below, so the
	// delivered snapshot is provably all-primitive (a malformed caller can't inject
	// a mutable object reference where a string is typed). Stricter than the two
	// sibling channels' truthiness checks, deliberately: this is the convergence
	// boundary every producer funnels through.
	if (typeof attachmentId !== 'string' || !attachmentId) return;
	if (typeof itemId !== 'string' || !itemId) return;
	if (typeof hostToken !== 'string' || !hostToken) return;
	if (typeof workspaceSlug !== 'string' || !workspaceSlug) return;
	if (!Array.isArray(rawImages)) return;
	// Capture the length ONCE too, and validate it — completing the read-once
	// invariant. `Array.isArray` sees through a Proxy to its target, so a length
	// getter that answered differently for the range check, the allocation and the
	// loop bound could otherwise diverge the three. A valid set length is a
	// positive integer.
	const count = rawImages.length;
	if (!Number.isInteger(count) || count <= 0) return;
	// The index must name a real position. `Number.isInteger` rejects NaN, a float
	// and ±Infinity before the range compare.
	if (!Number.isInteger(index) || index < 0 || index >= count) return;

	// Validate AND snapshot every record in ONE indexed pass, reading each field
	// EXACTLY ONCE into a fresh plain record. Reading once closes the getter/proxy
	// TOCTOU gap (a field that validates as a primitive cannot then project as an
	// object), the indexed `for` (not `.map`) sidesteps a forged array with a
	// shadowed non-callable `map`, and building fresh records from the captured
	// primitives means no hole, prototype value, shared reference or stray
	// property can reach a host. A malformed entry aborts the whole emission —
	// this is a SHAPE check, not the retired MIME gate (a null `mime_type` is
	// valid and passes).
	const images: LightboxImage[] = new Array(count);
	for (let i = 0; i < count; i++) {
		const raw = rawImages[i] as Record<string, unknown> | null | undefined;
		if (!raw || typeof raw !== 'object') return;
		const id = raw.id;
		const alt = raw.alt;
		const filename = raw.filename;
		const mime_type = raw.mime_type;
		const size_bytes = raw.size_bytes;
		const width = raw.width;
		const height = raw.height;
		if (typeof id !== 'string' || typeof alt !== 'string') return;
		if (!isStringOrNull(filename) || !isStringOrNull(mime_type)) return;
		if (!isNumberOrNull(size_bytes) || !isNumberOrNull(width) || !isNumberOrNull(height)) return;
		images[i] = { id, alt, filename, mime_type, size_bytes, width, height };
	}

	// The event's own invariant, ENFORCED rather than merely documented: the
	// opened index names the opened attachment. Read from the SNAPSHOT, so the
	// comparison and the delivered value are the same captured id.
	const target = images[index];
	if (target.id !== attachmentId) return;
	// A flat seed that CONTRADICTS the record it describes is a producer bug —
	// reject rather than let the surface choose between two disagreeing captions.
	// A null OR absent (`undefined`) seed asserts nothing — the nullish check
	// treats both as "not provided", matching the family's undefined-and-null-alike
	// posture; the snapshot below normalizes an absent seed to null.
	if (seedFilename != null && seedFilename !== target.filename) return;
	if (seedMime != null && seedMime !== target.mime_type) return;
	if (seedSize != null && seedSize !== target.size_bytes) return;

	// Deliver an explicit projection of ONLY the declared fields — no stray or
	// shared property from the caller's event object rides along. `invoker` is the
	// one intentional live reference (the focus target).
	const snapshot: AttachmentSurfaceOpenEvent = {
		attachmentId,
		workspaceSlug,
		itemId,
		hostToken,
		images,
		index,
		invoker,
		filename: seedFilename ?? null,
		mime_type: seedMime ?? null,
		size_bytes: seedSize ?? null,
	};
	for (const fn of surfaceListeners) fn(snapshot);
}
