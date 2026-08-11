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
 * Mint the identity for ONE `ItemDetail` mount. Call it once per host and
 * pass the result to every attachment surface that host owns — the strip, the
 * body `Editor`, every `CommentEditor`. One token per host, NOT one per
 * component: surfaces of the same host must be indistinguishable to the surface
 * host, while the master and peeked panes must never be.
 */
export function createAttachmentHostToken(): string {
	const c = typeof globalThis !== 'undefined' ? globalThis.crypto : undefined;
	if (c && typeof c.randomUUID === 'function') return `apanel-${c.randomUUID()}`;
	return `apanel-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
}

/**
 * The image / attachment record shape carried by the attachment surface channel.
 *
 * Every metadata field except `id` / `alt` is NULLABLE: an emitter knows only
 * what its own surface gives it, and an inline image's HEAD probe may not have
 * completed or may have failed, while an upload event carries only the
 * `UploadedAttachment` fields (which include the pixel dimensions, threaded for
 * the DR-5b policy — TASK-2459).
 *
 * `mime_type` is not decoration: it is what lets a CONSUMER re-state the DR-16
 * raster-safety gate over a whole set — deciding the RENDERER arm (raster bytes
 * vs the icon / download fallback) per entry rather than trusting the one element
 * that was clicked (TASK-2431). `width` / `height` feed phase 3b's pixel-based
 * loading policy.
 *
 * This is the ONLY declaration of the shape. `Lightbox.svelte` used to carry its
 * own `{id, alt}` twin; it now imports this one, so the component's props and the
 * channel's payload cannot drift.
 */
export interface LightboxImage {
	id: string;
	alt: string;
	/**
	 * Metadata the surface may caption with, all NULLABLE for the same reason the
	 * flat seeds are: an emitter knows only what its own surface gives it, and an
	 * inline image's HEAD probe may not have completed or may have failed, while an
	 * upload event carries only the `UploadedAttachment` fields (which now include
	 * the pixel dimensions, threaded for the DR-5b policy — TASK-2459).
	 *
	 * `mime_type` is not decoration: it is what lets a CONSUMER re-state the DR-16
	 * raster-safety gate over a whole set — which RENDERER arm each entry takes —
	 * rather than trusting the one element that was clicked (TASK-2431). `width` /
	 * `height` are here ahead of any
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

/**
 * Unified attachment surface (PLAN-2392 phase 3c-ii, TASK-2485).
 *
 * 3c-i had two ways to open an attachment: an options PANEL (a file) and an
 * image VIEWER (an image), each its own channel. 3c-ii converged them onto ONE
 * surface — a grown `Lightbox` that opens ANY attachment, image or file or a row
 * whose type is not yet resolved — and THIS is the only open channel now (the two
 * legacy channels, their predicates, and their resolved-MIME image type were
 * removed in T4b once every producer had repointed here). It is the SOLE
 * open channel, and it keeps the SAME host token (one per `ItemDetail` mount, not
 * one per channel) the deletion / upload channels use, so a host addresses every
 * channel with the identity it already has.
 *
 * TWO THINGS IT DELIBERATELY DOES DIFFERENTLY FROM THE RETIRED VIEWER CHANNEL:
 *
 *  1. NO ADMISSION MIME GATE. The old image-only channel failed the whole
 *     emission closed on an unresolved or non-allowlisted MIME, because it only
 *     ever opened IMAGES and an unviewable image is a silent no-op. The converged
 *     surface opens files and unresolved rows too, so admission cannot depend on
 *     the MIME: the allowlist governs the SLOT ARM instead (`getSurfaceRenderer`
 *     picks the raster viewer vs the icon / download fallback), never whether
 *     the surface opens. A null `mime_type` is data here, not a reason to drop —
 *     the old gate's per-emission drop becomes per-entry pass-through.
 *
 *  2. NO `anchor`. The panel positioned itself against an anchor element; the
 *     converged surface is centered (role=dialog on both breakpoints, AM-4), so
 *     the only element it needs is the focus-return target — `invoker`, exactly
 *     as the retired viewer channel already carried.
 *
 * IT SNAPSHOTS THE SET AT EMISSION. The retired channels passed the caller's
 * event and array straight through. This one is the convergence point every
 * producer funnels through, and some hand over a set they still hold and mutate
 * (a strip re-sorting, a timeline re-deriving). A caller that mutates its array —
 * or a record inside it — after calling must not be able to reach into an open
 * surface, so the array AND each record are copied here. A shallow array copy is
 * not enough: the records stay shared references until each is spread.
 *
 * MUTATION INTO AN OPEN SET IS OUT OF SCOPE, BY CONTRACT (PLAN-2392 3c-iii U4 /
 * TASK-2513). Scoped to THIS channel — the set as delivered by an emit: this
 * channel adds no NEW member to an already-open set after emit, and no snapshotted
 * record is mutated in place. (Downstream, a member's nullable display metadata may
 * still be COMPLETED by probing — `surfaceMetadata` fills a null `mime_type` /
 * `size_bytes` from a HEAD, the Lightbox normalizes a blank filename — augmenting
 * what an already-present member shows, not adding or swapping one.) The one
 * post-emit change to the set is member REMOVAL via DELETION reconciliation, which
 * reaches the surface two ways — the separate delete channel
 * (`registerAttachmentDeletionListener`) and the surface's own authoritative HEAD
 * 404 while probing — and can only RETIRE a member, never add one; how the Lightbox
 * then renders that (advance, close, or a sole-member missing state) is its own
 * concern, documented there. Two consequences, both deliberate and pinned rather
 * than assumed:
 *   - UPLOAD during an open surface does NOT join that surface's set. The strip's
 *     own tile list gains the row (its upload subscription), but the open surface
 *     keeps the emit-time set — proven end-to-end in
 *     `ItemAttachmentStrip.svelte.test.ts` ("upload during an open surface"). The
 *     complementary half — a producer mutating its OWN array/records in place
 *     after emit can't reach the surface — is the deep copy above, pinned directly
 *     by this module's deep-snapshot test.
 *   - A RENAME / metadata-change to a member cannot reach an open surface either,
 *     because THE CHANNEL FOR IT DOES NOT EXIST YET. `api.attachments` has no
 *     rename / update-in-place op — nothing edits an existing attachment's
 *     metadata; its closest operation, `transform`, mints a NEW peer attachment
 *     row rather than editing the source's metadata in place — and attachment
 *     metadata is otherwise treated as immutable. So there is no in-process channel
 *     by which a rename could reach an open surface; a member's caption could go
 *     stale only via an out-of-band DB change nothing observes. There is no test for
 *     this: nothing in the current tree can exercise it. If a rename op ever ships
 *     it needs its own bus channel plus open-set reconciliation rules — routed to
 *     IDEA-2515.
 */
export interface AttachmentSurfaceOpenEvent {
	/** UUID of the attachment the surface opens ON. */
	attachmentId: string;
	/**
	 * Workspace the attachments are read from, CAPTURED AT EMIT — never read live
	 * from the host, for the reason the retired viewer channel captured it too:
	 * the pane switches workspace without remounting, so a host that
	 * resolved the slug at render time could serve a surface opened in ws1 from
	 * ws2's endpoint. REQUIRED — there is no host fallback.
	 */
	workspaceSlug: string;
	/**
	 * UUID of the item whose `ItemDetail` mount should SHOW the surface. ROUTING,
	 * not ownership: it names the host in front of the user; it asserts nothing
	 * about which item the attachment belongs to, and nothing about permission.
	 * The two can genuinely differ — the comment composer is reused across an item
	 * switch, so a chip in an unsubmitted draft can be tapped while the pane shows a
	 * different item, and it correctly routes to the host in front of the user.
	 * Attachment authorization is the SERVER's, per attachment, against that
	 * attachment's own parent item; what the host supplies locally
	 * (`mutationsEnabled`) only decides whether to OFFER a mutation.
	 */
	itemId: string;
	/** Identity of the `ItemDetail` mount that owns the emitting surface. */
	hostToken: string;
	/**
	 * The set the surface pages through, in the emitting surface's own order.
	 * MIME-NULLABLE (`LightboxImage`): a file or an unresolved row is a
	 * first-class member here, so the type must admit a null `mime_type`. Readonly because the
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
 * The RULE is DR-8's, stated once in `isAddressable`: both sides fully
 * addressable, both fields equal. Accepts a null / undefined event.
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
 * The identity / workspace / non-empty-set guards are the retired viewer
 * channel's, unchanged: an emission missing an identity field reaches nobody or
 * invites a "matches anything" reading of the predicate; one missing the
 * workspace opens a surface whose every URL 404s (the slug is a path segment, and
 * the host does not substitute its own); an empty set opens a surface showing
 * nothing. The `Array.isArray` + indexed-loop posture is inherited too — an
 * array-like or a sparse array is caller input at a boundary, not a runtime
 * guarantee a type can make.
 *
 * WHAT DIFFERS from that retired channel:
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
	// type is not a runtime guarantee for a shared module" is this file's own rule.
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
