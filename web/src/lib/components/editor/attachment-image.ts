/**
 * AttachmentImage — Tiptap node for `pad-attachment:UUID` image references.
 *
 * Stores the attachment UUID (not a backend URL) so item content survives
 * a storage-backend migration untouched. See DOC-865 for the architecture
 * and `web/src/lib/markdown/attachments.ts` for the read-only render path
 * (the same UUID rendering is implemented there for shared/exported items).
 *
 * Node shape:
 *   - `uuid`: string — the attachment row's UUID (required)
 *   - `alt` : string | null — the alt text from `![alt](pad-attachment:UUID)`
 *
 * DOM contract — produced by both this extension's renderHTML AND by
 * markdown-it when it tokenizes `![alt](pad-attachment:UUID)`:
 *
 *   <img data-attachment-id="UUID" src="…?variant=thumb-md" alt="…">
 *
 * The two `parseHTML` rules below cover both forms — the editor's own
 * render path (data-attachment-id) and the markdown round-trip path
 * (src starts with `pad-attachment:`). Either one normalizes back to a
 * single attachmentImage node with the canonical attributes.
 *
 * Markdown serialization is opt-in via tiptap-markdown's `addStorage`
 * hook — emitting `![alt](pad-attachment:UUID)` keeps round-trips
 * idempotent. Without this storage, tiptap-markdown would fall back to
 * HTMLNode passthrough and output literal `<img …>` tags.
 */

import { Node, mergeAttributes } from '@tiptap/core';
import type { Node as ProseMirrorNode } from '@tiptap/pm/model';
import { canOpenInViewer } from '$lib/attachments/display';
import {
	type AttachmentUrlBuilder,
	type AttachmentVariant,
	fetchAttachmentMetadata,
	invalidateAttachmentMetadata,
	revalidateAttachmentMetadata,
	mimeToFormat
} from './attachment-metadata';
import { openCropModal, type CropResult } from './attachment-crop-modal';
import {
	notifyAttachmentSurfaceOpen,
	registerAttachmentDeletionListener,
	registerAttachmentParentRestoredListener
} from '$lib/attachments/events';
import {
	type AttachmentHostAddressReader,
	readUnaddressed
} from '$lib/attachments/hostAddress';
import type { AttachmentTransformRequest, AttachmentTransformResult } from '$lib/types';

// Re-export the shared types so existing call sites keep working.
export type { AttachmentVariant, AttachmentUrlBuilder };

/**
 * Live toolbars register a refresher here when constructed; the
 * editor calls notifyAttachmentImageCapabilitiesChanged() after
 * capabilities resolve, which in turn re-runs every refresher with
 * the latest supportedFormats list. Without this, a user who
 * selected an image before the capabilities fetch returned would
 * see an indefinitely-disabled toolbar — the existing toolbar DOM
 * would be stuck in "no processor" state until the NodeView was
 * destroyed and recreated. Registry-based push fixes that.
 *
 * Module-level rather than per-extension storage so a shared editor
 * surface (multiple Editor instances on one page) only has to fan
 * out once. Registrations are torn down via the returned dispose
 * function when the NodeView's destroy callback fires.
 */
const toolbarRefreshers = new Set<() => void>();

function registerToolbarRefresher(fn: () => void): () => void {
	toolbarRefreshers.add(fn);
	return () => toolbarRefreshers.delete(fn);
}

/**
 * Re-run every active toolbar's refresh hook. Called by Editor.svelte
 * once `api.server.capabilities()` resolves, so that any toolbar sitting
 * in the "all-disabled" pre-capabilities state re-reads the formats
 * reader and snaps to its correct per-format gating.
 *
 * The push is about EXISTING toolbar DOM only — the value itself is read
 * live through `options.supportedFormats()`, so a toolbar built after
 * capabilities resolved is already correct without a notification.
 */
export function notifyAttachmentImageCapabilitiesChanged(): void {
	for (const fn of toolbarRefreshers) fn();
}

/**
 * Reads the image formats the SERVER's processor supports, at the moment
 * the toolbar needs them.
 *
 * A reader, not a `string[]`, for the reason `$lib/attachments/hostAddress`
 * spells out at length: the list is only known after an async
 * `/server/capabilities` fetch that resolves LONG after `configure()`, and
 * Tiptap's `options` is a GETTER returning a fresh spread per access
 * (`@tiptap/core@3.22.5`), so `ext.options.supportedFormats = caps` mutates
 * a temporary that is thrown away on the next line. The NodeView compounds
 * it by snapshotting `this.options` once at construction. Both look like
 * they work; together they left the rotate/crop toolbar permanently in its
 * degraded "no image processor" state (BUG-2426).
 *
 * The host supplies a closure over its own live capability state, so the
 * value is current whenever it is read — no write ever has to land.
 */
export type SupportedFormatsReader = () => string[];

/**
 * Default option value: no processor. An editor that never wires
 * capabilities gets the degraded toolbar, which is the honest answer —
 * and is what `CommentEditor` configures DELIBERATELY, since comments
 * do not offer transforms at all.
 */
export const readNoSupportedFormats: SupportedFormatsReader = () => [];

/**
 * Result of a server-side image transform. The editor uses the new
 * row's id + dimensions to swap the node's attrs without a follow-up
 * GET — same shape as the upload response.
 */
export type AttachmentTransform = (
	uuid: string,
	payload: AttachmentTransformRequest
) => Promise<AttachmentTransformResult>;

export interface AttachmentImageOptions {
	HTMLAttributes: Record<string, unknown>;
	/**
	 * Resolves an attachment UUID to a download URL. Default implementation
	 * returns the literal `pad-attachment:UUID` reference — sufficient for
	 * markdown round-trip, but the editor will configure it to the actual
	 * `/api/v1/workspaces/{ws}/attachments/{id}` endpoint so images render.
	 */
	getDownloadUrl: AttachmentUrlBuilder;
	/**
	 * Workspace slug resolved at CONFIGURE time.
	 *
	 * The MIME probes that gate the rotate toolbar read
	 * `address().workspaceSlug` instead: this editor can outlive a pane
	 * workspace switch, and that value keys the metadata cache. An empty
	 * address workspace disables the probe — the toolbar still shows, but
	 * skips per-format gating.
	 */
	workspaceSlug: string;
	/**
	 * Reads the host address (item + owning `ItemDetail` mount) to stamp on
	 * attachment surface open events (PLAN-2392 DR-8). A reader rather than two
	 * strings — see `$lib/attachments/hostAddress` for why writing options
	 * after configure cannot work.
	 */
	address: AttachmentHostAddressReader;
	/**
	 * Reads the image formats the server-side processor supports. Drives
	 * the rotate toolbar's enabled state per attachment: a button is
	 * disabled (with tooltip) when the image's MIME isn't in the list.
	 * Empty list ⇒ all transforms disabled (degraded build, or a surface
	 * like the comment composer that switches transforms off on purpose).
	 *
	 * A reader rather than a list because the list arrives from an async
	 * capabilities fetch after `configure()` — see
	 * `SupportedFormatsReader` above for why writing it in afterwards
	 * cannot work (BUG-2426).
	 */
	supportedFormats: SupportedFormatsReader;
	/**
	 * Calls the server's /transform endpoint. Set by Editor.svelte to
	 * `api.attachments.transform(workspaceSlug, uuid, payload)`.
	 * Defaulting to a thrown error means a misconfigured editor
	 * fails loudly when the user clicks rotate, rather than silently
	 * swallowing the click.
	 */
	transform: AttachmentTransform;
	/**
	 * Optional error sink the editor can wire to its toast / logger.
	 * Receives the user-facing message; the toolbar already handled
	 * the technical error before calling this.
	 */
	onError?: (message: string) => void;
}

declare module '@tiptap/core' {
	interface Commands<ReturnType> {
		attachmentImage: {
			/**
			 * Insert an attachment image at the current selection.
			 * Used by the upload plugin in TASK-875.
			 */
			setAttachmentImage: (options: { uuid: string; alt?: string | null }) => ReturnType;
		};
	}
}

const PAD_ATTACHMENT_PREFIX = 'pad-attachment:';

/** Escape `[` and `]` in alt text so the markdown serializer's brackets stay balanced. */
function escapeMarkdownAlt(s: string): string {
	return s.replace(/[\[\]]/g, (m) => '\\' + m);
}

export const AttachmentImage = Node.create<AttachmentImageOptions>({
	name: 'attachmentImage',

	// Inline atom — same shape as a regular Image. `atom: true` prevents
	// keyboard navigation from putting the cursor *inside* the node, so
	// Backspace/Delete remove it as a single unit.
	group: 'inline',
	inline: true,
	atom: true,
	selectable: true,
	draggable: true,

	addOptions() {
		return {
			HTMLAttributes: {},
			getDownloadUrl: (uuid: string) => `${PAD_ATTACHMENT_PREFIX}${uuid}`,
			workspaceSlug: '',
			address: readUnaddressed,
			supportedFormats: readNoSupportedFormats,
			transform: async () => {
				throw new Error('AttachmentImage: configure({ transform }) is required to use rotate/crop');
			},
			onError: undefined,
		};
	},

	addAttributes() {
		return {
			uuid: {
				default: null,
				parseHTML: (element) => {
					// Editor render output uses data-attachment-id; markdown-it's
					// image token output uses src=pad-attachment:UUID. Accept both
					// so the round-trip lands on the same canonical node.
					const dataId = element.getAttribute('data-attachment-id');
					if (dataId) return dataId;
					const src = element.getAttribute('src') ?? '';
					if (src.startsWith(PAD_ATTACHMENT_PREFIX)) {
						return src.slice(PAD_ATTACHMENT_PREFIX.length);
					}
					return null;
				},
				renderHTML: (attrs) => (attrs.uuid ? { 'data-attachment-id': attrs.uuid } : {}),
			},
			alt: {
				default: null,
				parseHTML: (element) => element.getAttribute('alt'),
				renderHTML: (attrs) => (attrs.alt ? { alt: attrs.alt } : {}),
			},
		};
	},

	parseHTML() {
		return [
			// Canonical editor form — produced by our own renderHTML and also
			// by Codex / external HTML pastes that include the data attribute.
			{ tag: 'img[data-attachment-id]' },
			// Markdown round-trip form — produced when markdown-it renders
			// `![alt](pad-attachment:UUID)` to an <img>.
			{ tag: 'img[src^="pad-attachment:"]' },
		];
	},

	renderHTML({ HTMLAttributes, node }) {
		const uuid = (node.attrs.uuid as string | null) ?? '';
		const src = uuid ? this.options.getDownloadUrl(uuid, 'thumb-md') : '';
		return [
			'img',
			mergeAttributes(this.options.HTMLAttributes, HTMLAttributes, {
				src,
				loading: 'lazy',
				class: 'attachment-image',
			}),
		];
	},

	/**
	 * NodeView wraps the <img> in a positioned <span> so we can layer a
	 * rotate toolbar on top of it when the node is selected. The
	 * toolbar's per-button enabled state is recomputed when the
	 * image's MIME resolves via the metadata cache — disabled state
	 * carries an explanatory tooltip so the user knows why
	 * rotation is unavailable on their build.
	 *
	 * Tiptap's renderHTML still emits a bare <img> for clipboard /
	 * `getHTML()` / SSR paths; only the live editor view goes through
	 * this NodeView.
	 */
	addNodeView() {
		return ({ node, editor, getPos }) => {
			const opts = this.options;
			const wrapper = document.createElement('span');
			wrapper.className = 'attachment-image-wrapper';
			wrapper.setAttribute('contenteditable', 'false');

			const img = document.createElement('img');
			img.classList.add('attachment-image');
			img.loading = 'lazy';
			// Mutable closure state — kept in sync via update() so attr
			// changes (rotate/crop swap, future peer Yjs ops, etc.) refresh
			// the live <img> without forcing ProseMirror to destroy and
			// recreate the NodeView (which would flicker locally and jump
			// the cursor under live collab).
			let currentUuid = (node.attrs.uuid as string | null) ?? '';
			let currentAlt = (node.attrs.alt as string | null) ?? '';
			if (currentUuid) {
				img.setAttribute('data-attachment-id', currentUuid);
			}
			if (currentAlt) img.alt = currentAlt;

			// Missing-attachment placeholder (PLAN-2382 / TASK-2384).
			//
			// The markdown render path already degrades a deleted attachment to
			// an explicit placeholder (markdown/attachments.ts::renderAttachmentMissing)
			// rather than a broken-image glyph, because the glyph reads as a
			// transient network failure when the state is actually permanent.
			// The live NodeView had no equivalent: it assigned img.src and left
			// the browser to paint whatever a 404 produces.
			//
			// That gap became user-visible when the item attachment strip gained
			// a delete affordance — deleting an image still referenced in the
			// body must show the placeholder IMMEDIATELY, not after a reload.
			const missing = document.createElement('span');
			missing.className = 'attachment-missing';
			missing.title = 'This attachment could not be loaded — it may have been deleted. Click to retry.';
			missing.style.display = 'none';

			// Latched by a confirmed deletion (not by a mere load failure).
			// Deletion is authoritative: a load still in flight when it lands
			// must not be allowed to paint the image back (Codex round 15).
			//
			// The restore channel foreseen for PLAN-2411 now EXISTS (BUG-2509): the
			// bus carries a parent-restore signal and this file subscribes to it
			// below. What that channel does NOT do is clear the latch on the signal
			// itself — it re-probes, and only an authoritative `ok` from the server
			// clears anything. The DR-17 rule is unchanged and still holds: Ctrl-Z
			// must leave an inert placeholder rather than resurrect a working
			// attachment, because the delete was a REST row mutation Tiptap/Yjs
			// history cannot roll back. Nothing in this file clears the latch on a
			// DOCUMENT event; the only two things that clear it are a uuid swap (a
			// different target entirely) and a server `ok`.
			let deleted = false;
			// True once the NodeView is torn down. Async continuations (HEAD
			// probes, transform results) must not touch DOM after that.
			let destroyed = false;

			// The MIME this node's attachment is KNOWN to have, from a HEAD probe. Null
			// until one has answered — the probe is lazy (toolbar construction, uuid
			// swap), so "null" means "not yet asked", never "not an image".
			//
			// Declared HERE rather than next to its first reader so that
			// applyImageSemantics() — which the load/missing paths call — can read it
			// without a temporal-dead-zone hazard.
			let knownMime: string | null = null;

			// True while an activation's MIME resolution is in flight. See
			// activate() for why one is enough — and why "exactly once" now needs
			// a latch rather than only the key-repeat guard.
			//
			// The counter is what makes RELEASING it safe. The latch is dropped in
			// two places — the resolution's own finalizer, and a uuid swap, which
			// must not leave the NEW image latched behind the old one's request —
			// and an unconditional finalizer would let a superseded activation
			// release a latch that a LATER one is holding. Each activation stamps
			// its own number and releases only if it is still the current holder.
			let activating = false;
			let activationSeq = 0;

			/**
			 * DR-12: the inline body image is an activation target, so it carries a
			 * button's semantics — but ONLY while it is actually one.
			 *
			 * The three states that make it not one:
			 *
			 *   - no uuid — activate() has nothing to open;
			 *   - a confirmed deletion, or any load failure that swapped the
			 *     placeholder in — the <img> is hidden and activate() refuses, so
			 *     role+tabindex would be a focus stop that announces itself as a
			 *     button and does nothing. (The placeholder itself carries the
			 *     retry affordance while the failure is still transient.)
			 * A KNOWN MIME outside the raster allowlist is NOT one of them, as of
			 * TASK-2434. It used to be: the gate was binary — viewer or nothing —
			 * so a resolved `image/svg+xml` made the image a control that refused,
			 * and the semantics had to come off. Since the 3c-ii convergence a
			 * non-allowlisted attachment still opens — on the converged surface's
			 * no-bytes / options fallback arm — so the image stays a perfectly real
			 * activation target; only which arm the surface picks changes. Taking the
			 * semantics off now would hide a working control instead of retiring a
			 * dead one, and would break it for the mouse too, since click and key
			 * share one gate: the first tap would open the surface, resolve the MIME,
			 * and leave the image inert for every tap after it.
			 *
			 * The accessible name comes from alt with a GENERIC fallback: there is
			 * no filename on the node's attrs and the HEAD metadata does not carry
			 * one either, so the filename form DR-12 sketches has no source here.
			 * It also has to name the AFFORDANCE rather than always promising a
			 * viewer — a resolved non-raster type announces the options affordance
			 * (the surface's fallback arm) it actually opens.
			 */
			/**
			 * Is this image an activation target right now?
			 *
			 * ONE predicate, read by activate() AND by applyImageSemantics(),
			 * because "can be opened" and "announces itself as openable" are the
			 * same question and a dead focus stop is exactly what they look like
			 * when they disagree. Two copies would drift: the load-failure clause
			 * below was, briefly, only in the semantics half — so the placeholder
			 * hid the image and stripped its role, while a stale or synthetic
			 * event on the hidden <img> still opened a viewer.
			 *
			 * It answers "does activation DO something", not "which arm it opens on".
			 * The MIME is deliberately absent from here: activation emits ONE surface
			 * regardless (the surface's renderer picks the arm), and this predicate is
			 * also what a post-await continuation re-checks — folding a MIME clause in
			 * would make it self-invalidate the moment activation wrote the MIME.
			 */
			function canActivate(): boolean {
				return (
					!!currentUuid &&
					!deleted &&
					// Hidden means the placeholder has taken over: either a
					// confirmed deletion or a load failure. Neither has an image
					// to show, so neither has anything to open.
					img.style.display !== 'none'
				);
			}

			/**
			 * What activation would open, as far as anything KNOWN says. Unprobed
			 * reads as the viewer — "not yet asked" is not "not viewable", and the
			 * probe is lazy — which is exactly what the name has to say before the
			 * HEAD lands. activate() never trusts it: it resolves the MIME itself.
			 */
			function announcesPanel(): boolean {
				return !!knownMime && !canOpenInViewer(knownMime);
			}

			function applyImageSemantics() {
				if (!canActivate()) {
					// Removing tabindex from the focused element would strand focus on
					// something no further keystroke can leave.
					if (document.activeElement === img) img.blur();
					img.removeAttribute('role');
					img.removeAttribute('tabindex');
					img.removeAttribute('aria-label');
					return;
				}
				img.setAttribute('role', 'button');
				img.setAttribute('tabindex', '0');
				img.setAttribute(
					'aria-label',
					announcesPanel()
						? currentAlt
							? `Attachment options: ${currentAlt}`
							: 'Attachment options'
						: currentAlt
							? `View image: ${currentAlt}`
							: 'View attachment image'
				);
			}

			/**
			 * The pending contract, minimal (TASK-2434).
			 *
			 * Activation awaits a HEAD. On a cache hit that is one microtask and
			 * nothing is visible; cold, it is a round trip during which a click
			 * that does nothing reads as broken. This NodeView has no
			 * metadata-pending affordance — the placeholder below is for LOAD
			 * failure, a different thing — so the smallest honest one: `aria-busy`
			 * for assistive tech and a wait cursor for everyone else. No spinner
			 * chrome in a parity commit.
			 */
			function setActivationPending(pending: boolean): void {
				if (pending) {
					img.setAttribute('aria-busy', 'true');
					img.style.cursor = 'progress';
					return;
				}
				img.removeAttribute('aria-busy');
				img.style.cursor = '';
			}

			function showMissing() {
				missing.textContent = `📎 ${currentAlt || 'Attachment unavailable'}`;
				// Distinct copy per cause: a confirmed deletion is permanent and
				// retry is blocked, so don't invite one.
				missing.title = deleted
					? 'This attachment has been deleted'
					: 'This attachment could not be loaded — it may have been deleted. Click to retry.';
				missing.style.cursor = deleted ? 'default' : 'pointer';
				// And the INTERACTIVE SEMANTICS go with the copy, not just the
				// cursor (DR-12; final review round 5). A confirmed deletion
				// makes `retryLoad` a no-op, so leaving role=button + tabindex
				// hands a keyboard or screen-reader user a focus stop that
				// announces itself as a button and does nothing — the same dead
				// stop the file chip's `disabled` closes, on the surface next to
				// it. A transient failure IS retryable and keeps both.
				if (deleted) {
					if (document.activeElement === missing) missing.blur();
					missing.removeAttribute('role');
					missing.removeAttribute('tabindex');
				} else {
					missing.setAttribute('role', 'button');
					missing.setAttribute('tabindex', '0');
				}
				if (currentUuid) missing.setAttribute('data-attachment-id', currentUuid);
				missing.style.display = '';
				img.style.display = 'none';
				// The <img> the placeholder replaces stops being a control too —
				// see applyImageSemantics for why a hidden/dead image must not
				// keep announcing itself as one.
				applyImageSemantics();
			}

			// Re-armed on every uuid swap in update() below, so a rotate/crop
			// (or a peer's op) that points the node at a live attachment clears
			// a stale placeholder instead of latching it.
			function resetMissing() {
				if (deleted) return;
				missing.style.display = 'none';
				img.style.display = '';
				applyImageSemantics();
			}

			// Load events carry no identity, and this NodeView outlives a uuid
			// swap (rotate/crop, or a collab peer's op). Comparing img.src at
			// event time cannot distinguish a QUEUED stale event, because the
			// src has already been swapped to the new uuid by then — the check
			// passes and a late failure for the OLD image hides the healthy new
			// one (Codex round 7 caught this in the round-3 fix).
			//
			// Instead, every load gets its own listener pair, detached when the
			// src changes. Removing a listener before the event is dispatched
			// to it prevents its invocation even if the event was already
			// queued, so a superseded load simply has no callback left to run.
			let detachLoadListeners = () => {};

			/**
			 * Latch the permanent placeholder for a row the server says is
			 * gone. Same end state as the deletion broadcast, reached by a
			 * different route: the broadcast only fires for a delete that
			 * happened in THIS tab's session, while this covers a node whose
			 * row was already gone when it rendered — which is exactly what
			 * editor undo produces (PLAN-2392 DR-17). Tiptap/Yjs history
			 * owns the document; the delete was a REST row mutation it can't
			 * roll back, so undo restores a node pointing at nothing.
			 */
			function latchMissing(forUuid: string, seqAtStart: number) {
				if (destroyed || deleted) return;
				if (!forUuid || currentUuid !== forUuid) return;
				// The staleness fence lives HERE, not at the call sites, and the
				// generation is a REQUIRED argument so it cannot be forgotten: every
				// path into this function is an async probe answering a question it
				// asked earlier, and three of them originally landed without a fence
				// (the two toolbar MIME probes and the activation probe — found in
				// review, after the first two were fixed one at a time). A latch is
				// destructive and permanent, so "the caller will remember" is the
				// wrong shape for it; capture `stateSeq` before your request and hand
				// it over, and a probe that predates a restore, a heal or a uuid swap
				// simply cannot re-kill the node.
				if (stateSeq !== seqAtStart) return;
				deleted = true;
				stateSeq += 1;
				// Same reason the deletion listener does this: an in-flight
				// load's `load` event would otherwise paint the image back.
				detachLoadListeners();
				showMissing();
			}

			/**
			 * An <img> `error` event carries no status code, so a deleted row
			 * and a network blip are indistinguishable at that layer — which
			 * is why every load failure has to stay retryable by default. A
			 * HEAD probe is what tells them apart: only a 404 latches, and a
			 * `transient` result leaves the retryable placeholder exactly as
			 * it was (and is not cached, so Retry re-issues the HEAD).
			 *
			 * It REVALIDATES rather than reading the cache: the failed load is
			 * evidence that whatever we last observed about this row is out of
			 * date, and a cached `ok` from before the deletion would make the
			 * placeholder permanently unlatchable.
			 */
			function probeForMissing(forUuid: string) {
				// Workspace off the READER, not the static option — it keys the
				// metadata cache and this editor can outlive a workspace switch
				// (final review round 3).
				const probeWs = opts.address().workspaceSlug;
				if (!forUuid || !probeWs || deleted || destroyed) return;
				const seqAtStart = stateSeq;
				void revalidateAttachmentMetadata(probeWs, forUuid, opts.getDownloadUrl).then(
					(result) => {
						// `latchMissing` applies the staleness fence itself; a restore
						// (or any other authoritative transition) since this HEAD went
						// out makes this answer unable to re-kill the node.
						if (result.status === 'missing') latchMissing(forUuid, seqAtStart);
					}
				);
			}

			function loadImage(url: string) {
				detachLoadListeners();
				const forUuid = currentUuid;
				const onError = () => {
					showMissing();
					probeForMissing(forUuid);
				};
				const onLoad = () => resetMissing();
				img.addEventListener('error', onError, { once: true });
				img.addEventListener('load', onLoad, { once: true });
				detachLoadListeners = () => {
					img.removeEventListener('error', onError);
					img.removeEventListener('load', onLoad);
				};
				img.src = url;
			}

			if (currentUuid) loadImage(opts.getDownloadUrl(currentUuid, 'thumb-md'));
			applyImageSemantics();

			/**
			 * This node's PRESENTATION generation (BUG-2509).
			 *
			 * Bumped by every authoritative transition — a deletion latched, a
			 * `missing` latched, a successful heal, a uuid swap, and the receipt of
			 * a restore signal.
			 *
			 * WHAT IT GOVERNS, precisely: the MISSING/present state — the latch and
			 * the heal. `latchMissing` takes the captured generation as a required
			 * argument and applies the fence itself, so every probe that can kill
			 * this node is covered by construction rather than by remembering:
			 *
			 *   a probe may only latch if nothing authoritative has happened since
			 *   it was issued.
			 *
			 * WHAT IT DOES NOT GOVERN: opening a viewer (`activationSeq` owns that —
			 * a separate question with a separate lifetime) and the rotate/crop
			 * transforms (fenced on uuid + editor state, and they mint a NEW
			 * attachment rather than re-presenting this one). An activation's 404 arm
			 * does latch, so it carries this generation too.
			 *
			 * A per-cause fence (only "did a delete land?") is not enough, and the
			 * first version of this fix proved it in both directions: a stale
			 * archived-window probe could re-latch a node the restore had already
			 * healed, and a value-compared uuid (`currentUuid === forUuid`) passes
			 * again after a swap AWAY AND BACK, letting a stale positive answer
			 * revive an attachment deleted in between.
			 */
			let stateSeq = 0;

			const disposeDeletionListener = registerAttachmentDeletionListener((deletedUuid) => {
				if (deletedUuid !== currentUuid) return;
				deleted = true;
				stateSeq += 1;
				// Drop the in-flight request's listeners: its `load` would
				// otherwise fire after the delete and restore the image.
				detachLoadListeners();
				showMissing();
				refresh();
			});

			/**
			 * Parent-item RESTORE (BUG-2509).
			 *
			 * The counterpart to the deletion listener above, and deliberately NOT
			 * its mirror image: that one is authoritative and latches, this one
			 * carries no verdict and only prompts a re-ask. Archiving an item 404s
			 * its attachments without deleting them, so a NodeView constructed
			 * inside the archived window probes, gets the same 404 a deletion
			 * gives, and latches `deleted` — which nothing then clears, because the
			 * latch is closure-private and only a uuid swap resets it. Restoring
			 * the parent left the node inert for the rest of the page's life.
			 *
			 * THE SERVER DECIDES, NOT THE SIGNAL. The re-probe is what clears the
			 * latch, and only on an authoritative `ok`:
			 *   - `missing`   — the row really is gone (deleted while the parent was
			 *                   archived, say). Stay latched; this is exactly the
			 *                   DR-17 case, and honouring the signal blindly here is
			 *                   what would turn restore into an undo-resurrection
			 *                   vector.
			 *   - `transient` — no evidence either way. Stay as we are.
			 * A signal that reaches the wrong node therefore costs one HEAD and
			 * changes nothing, which is what makes routing-by-itemId safe.
			 *
			 * `no-store`, for the reason the timeline's restore re-probe uses it
			 * (ItemTimeline's `pendingRestoreNoStore`): the endpoint sets
			 * `max-age=3600`, and this probe exists precisely to escape what was
			 * observed inside the archived window.
			 */
			const disposeRestoreListener = registerAttachmentParentRestoredListener((event) => {
				if (destroyed || !currentUuid) return;
				const addr = opts.address();
				if (!addr.workspaceSlug || !addr.itemId) return;
				if (event.workspaceSlug !== addr.workspaceSlug || event.itemId !== addr.itemId) return;
				// Nothing to heal unless a placeholder is actually showing — either
				// latched permanent or the retryable one (a probe that never landed,
				// or landed `transient`, inside the archived window).
				// A restore is an authoritative transition in its own right: every
				// observation made BEFORE it is now stale, including a probe still in
				// flight from the archived window. Bump before the early return below,
				// so the invalidation happens even when this node has nothing to heal.
				stateSeq += 1;
				if (!deleted && missing.style.display === 'none') return;
				const forUuid = currentUuid;
				const probeWs = addr.workspaceSlug;
				const seqAtStart = stateSeq;
				void revalidateAttachmentMetadata(probeWs, forUuid, opts.getDownloadUrl, {
					cache: 'no-store'
				}).then((result) => {
					// Fence on teardown and a uuid swap: this NodeView outlives a
					// rotate/crop, and healing a node that has moved on would paint the
					// previous attachment over the current one.
					if (destroyed || currentUuid !== forUuid) return;
					// And on the WORKSPACE, re-read live rather than trusted from
					// capture: this editor outlives a workspace switch, so an answer
					// about ws-A's copy must not heal a node now living in ws-B. Same
					// fence ItemTimeline's probe uses (`reqWs !== wsSlug`).
					//
					// The `itemId` half is deliberately NOT re-checked here (raised in
					// review; declined with reason). It is ROUTING, not ownership —
					// stated as such on the surface channel — and the answer is about
					// the ATTACHMENT, not the item: if this uuid is live in this
					// workspace, healing the node showing it is correct no matter which
					// item's host happens to be in front of the user. Fencing on it
					// would imply a relationship the code does not have.
					if (opts.address().workspaceSlug !== probeWs) return;
					// ANY authoritative transition since this HEAD went out wins over
					// it. The deletion case is the one that matters for DR-17: without
					// this, "restore probe starts → delete is confirmed and broadcast →
					// probe resolves ok" repaints a row the server no longer has.
					if (stateSeq !== seqAtStart) return;
					// A `missing` here is not "nothing to do" — it is the authoritative
					// existence answer this probe asked for, so it LATCHES (which also
					// settles two restore probes resolving out of order: the `missing`
					// one is not silently dropped just because an `ok` got there first).
					// `transient` is the only arm that says nothing and changes nothing.
					if (result.status === 'missing') {
						latchMissing(forUuid, seqAtStart);
						return;
					}
					if (result.status !== 'ok') return;
					deleted = false;
					stateSeq += 1;
					// Cache-bust for the same reason `retryLoad` does: the failed load
					// is in the browser's HTTP cache, and re-assigning the identical
					// URL can replay it instead of reaching the server.
					const base = opts.getDownloadUrl(forUuid, 'thumb-md');
					loadImage(`${base}${base.includes('?') ? '&' : '?'}restored=${Date.now()}`);
					// The placeholder's copy, cursor, role and tabindex were all set for
					// a permanent deletion; `refresh` re-gates the toolbar for a node
					// that is openable again.
					refresh();
					applyImageSemantics();
				});
			});

			// role/tabindex are set by showMissing(), which knows whether this is
			// a retryable failure or a confirmed deletion. Hidden and inert here.
			missing.style.cursor = 'pointer';

			function retryLoad() {
				// A confirmed deletion is not retryable — only a transient load
				// failure is.
				if (!currentUuid || deleted) return;
				resetMissing();
				// The cache-busting query is what makes a retry after a
				// transient failure actually reach the network instead of
				// replaying the failed cache entry.
				const base = opts.getDownloadUrl(currentUuid, 'thumb-md');
				loadImage(`${base}${base.includes('?') ? '&' : '?'}retry=${Date.now()}`);
			}
			/**
			 * The `transient` arm of the matrix (TASK-2434 / DR-17).
			 *
			 * `transient` says NOTHING about whether the row exists — a 5xx, a
			 * proxy hiccup, a network throw — so it must never latch and must
			 * never open. Before this task it also did nothing at all, which left
			 * the worst of the three: a focused image announcing itself as a button
			 * whose activation silently returned, with no way for the user to learn
			 * that anything failed or to try again. Repeat presses would have gone
			 * on doing nothing forever.
			 *
			 * So it hands over to the RETRYABLE placeholder — the one affordance
			 * this NodeView already has for "this did not work, click to retry",
			 * reached here by a different route than a failed `load`. Nothing is
			 * latched: `deleted` stays false, the transient result is not cached
			 * (`fetchAttachmentMetadata` evicts it on settle), and Retry re-issues
			 * both the image load and, on a second failure, the HEAD.
			 *
			 * Retry does NOT resume the activation, deliberately. It is the
			 * placeholder's existing affordance and it means "load this image
			 * again", not "open it" — a reload control that opened a viewer would
			 * be doing something the user did not ask for. The user gets the image
			 * back and activates it again if they still want to, and that second
			 * gesture really does re-probe: a transient result is evicted from the
			 * metadata cache as it settles, so nothing replays the failure.
			 *
			 * The cost, accepted: an image that had rendered FINE is replaced by
			 * the placeholder when only its HEAD failed. It is one click to undo
			 * and it is recoverable; a control that silently does nothing is
			 * neither. Anything gentler would be a third failure state — an inline
			 * error, a toast — which is new chrome this commit does not add.
			 */
			function showTransientProbeFailure(): void {
				// Keyboard activation is the case that matters: showMissing() takes
				// the semantics off the <img> and blurs it, so without this the
				// keypress that reported the failure also drops focus to <body>.
				// The placeholder is the retry control, so focus belongs on it.
				const hadFocus = document.activeElement === img;
				showMissing();
				if (hadFocus) missing.focus();
			}

			missing.addEventListener('click', retryLoad);
			missing.addEventListener('keydown', (event) => {
				if (event.key === 'Enter' || event.key === ' ') {
					event.preventDefault();
					retryLoad();
				}
			});

			/**
			 * THE single activation path for this node — every route that opens the
			 * image goes through here.
			 *
			 * The MIME resolution used to live inside the click handler, which made
			 * it a property of the MOUSE rather than of activation: a keyboard path
			 * that opened on its own would have bypassed it entirely. One function
			 * owns it so there is exactly one place a route can be added to and
			 * exactly one place the behaviour can be strengthened — which is what
			 * TASK-2433 did on this same seam: the surface emit, the MIME resolution
			 * and the address fence all live inside this one function.
			 *
			 * The gate itself is `canActivate()`, shared with the semantics pass so
			 * an image can never be openable and un-announced, or announced and
			 * dead.
			 *
			 * THE MIME IS RESOLVED BEFORE ANYTHING IS EMITTED (TASK-2433). It is no
			 * longer an ADMISSION gate — since 3c-ii the converged surface opens ANY
			 * resolved attachment and its own `getSurfaceRenderer` picks the arm
			 * (raster `<img>` vs the icon / download fallback for `image/svg+xml` and
			 * everything else) — but the probe is still where the answer is OBTAINED,
			 * and the emitted set's `mime_type` (plus `size`) needs it. Either the
			 * cache answers (the common case — the toolbar probe and the strip both
			 * warm the same entry) or we await one HEAD.
			 *
			 * Every probe result has a destination, and none of them is "return
			 * quietly":
			 *
			 *   - `ok` (any resolved type) → ONE surface emit; the surface arm sorts
			 *     raster from non-raster. `image/svg+xml` reaches the fallback arm
			 *     (active content, never rendered as bytes), not a raster viewer.
			 *   - `missing` (an authoritative 404) → the permanent placeholder,
			 *     latched. Nothing opens.
			 *   - `transient` → the RETRYABLE placeholder. Never an open, and
			 *     never a latch: only a 404 is authoritative (DR-17).
			 *
			 * The one remaining silent return is a surface with NO WORKSPACE to
			 * probe with (SSR / preview). It is not a dead stop in practice: those
			 * surfaces have no host mounted to receive the event, so there is
			 * nothing to route to and nothing to say.
			 *
			 * It also closes a mid-phase bypass Codex found: the old gate read
			 * `knownMime` only when truthy, so a click landing before the lazy
			 * probe resolved opened the original file in the legacy dialog, and a
			 * later `unsafe` answer did not close it.
			 */
			function activate(): void {
				if (!canActivate()) return;
				// One activation at a time. The MIME resolution is asynchronous
				// even on a cache hit (a settled promise still resolves on a
				// microtask), so two gestures inside one tick would otherwise both
				// clear the gate and emit — and "fires exactly once" has to survive
				// the await that was just introduced, not only the key repeat.
				if (activating) return;
				const forUuid = currentUuid;
				// The address the GESTURE happened at. The workspace half is what
				// keys the metadata cache and what the viewer reads every image URL
				// from, so probing under one workspace and emitting under another
				// would serve ws1's click from ws2's endpoint. Snapshot once, then
				// re-check at emit (below) rather than re-reading and trusting it.
				//
				// DESTRUCTURED INTO PRIMITIVES, not held as the returned object.
				// `opts.address()` is a reader the HOST supplies, and both live
				// implementations happen to build a fresh object literal per call —
				// so holding the reference would be safe today. It would be safe
				// only for that reason. A host that returned a stable object it
				// mutated in place would rewrite the very snapshot this fence
				// compares against, and the comparison would pass unconditionally
				// while looking exactly as it looks now. Three string copies buy
				// independence from a property no interface states and no test
				// could plausibly catch.
				const { workspaceSlug: fromWs, itemId: fromItem, hostToken: fromHost } =
					opts.address();
				// No workspace ⇒ no probe ⇒ nothing can be positively known. An
				// SSR/preview surface simply does not open a viewer.
				if (!fromWs) return;
				activating = true;
				const seq = ++activationSeq;
				// The PRESENTATION generation as well (BUG-2509). `activationSeq`
				// governs whether this gesture may still OPEN something; it says
				// nothing about whether this answer may still LATCH, and the 404 arm
				// below does exactly that. A probe issued while the parent was
				// archived would otherwise re-kill an image the restore has healed.
				const seqAtStart = stateSeq;
				setActivationPending(true);
				void fetchAttachmentMetadata(fromWs, forUuid, opts.getDownloadUrl)
					.then((result) => {
						// Everything the gate asserted at gesture time has to still
						// hold at emit time: the NodeView can be torn down, the node
						// can be pointed at a different attachment (rotate/crop, a
						// peer's op), and the deletion broadcast can land — all
						// inside the await window.
						if (destroyed || currentUuid !== forUuid) return;
						// And this must still be the CURRENT activation. Comparing
						// the uuid is not enough on its own: the node can be pointed
						// away and back again (a rotate the user undoes, a peer's op
						// reverted), and then a request from before the round trip
						// finds its own uuid in place and emits for a gesture two
						// swaps ago.
						if (activationSeq !== seq) return;
						// DELETION, checked explicitly and BEFORE the result is read.
						// It is the one invalidation the uuid comparison structurally
						// cannot catch: a delete does not change which attachment the
						// node points at, so a probe issued before it can resolve `ok`
						// afterwards with `forUuid` still current — describing a row
						// the server has since dropped. `canActivate()` restates this
						// below; it is spelled out here because the branches between
						// the two must not act on that answer either.
						//
						// The two guards happen to be equivalent TODAY — every path
						// that sets `deleted` also hides the image — but nothing
						// enforces that, and inferring "the row is gone" from "the
						// placeholder is showing" would silently stop holding the
						// moment a deletion state exists that does not hide.
						if (deleted) return;

						// AUTHORITATIVE 404. The row is gone: latch the permanent
						// placeholder (the same end state the deletion broadcast
						// reaches) and open nothing. This runs even when the image is
						// already hidden behind a retryable placeholder — upgrading a
						// transient failure to a confirmed one is exactly what a 404
						// is for, so it deliberately precedes the presentability
						// check below.
						if (result.status === 'missing') {
							latchMissing(forUuid, seqAtStart);
							return;
						}
						// NOT AUTHORITATIVE. Stay retryable, latch nothing, open
						// nothing — and stop being a control that silently does
						// nothing (see showTransientProbeFailure).
						if (result.status === 'transient') {
							showTransientProbeFailure();
							return;
						}

						// A positively-known MIME. Record it: the semantics pass and
						// the transform toolbar both read `knownMime`, and this
						// activation is often the FIRST thing to learn it (the
						// toolbar's own probe only runs once the node is selected).
						// Without this the image would keep announcing "View image"
						// for something that opens on the surface's fallback arm.
						//
						// The SEMANTICS follow, and deliberately nothing else. The
						// rotate/crop toolbar reads `knownMime` too, but refreshing
						// it from here would settle its per-format gating earlier
						// than it does today — a behaviour change this task's
						// contract does not ask for. It is also unnecessary: a
						// toolbar only exists once the node has been selected, and
						// selection runs its own probe for the same uuid which
						// refreshes on arrival. The window is a moment of staleness
						// that closes itself.
						knownMime = result.mime;
						applyImageSemantics();

						// The gesture-time gate, restated on state that may have
						// moved: a load failure inside the await window hands over to
						// the placeholder, and there is then no image to act on.
						if (!canActivate()) return;

						// The host may have MOVED while the HEAD was in flight — the
						// comment composer is deliberately reused across an item
						// switch (see hostAddress.ts), so its address is live. The
						// gesture belonged to the old address: emitting there opens a
						// surface over a pane the user has left, and emitting at the
						// new one attributes the gesture to a different item. Neither
						// is what the user did, so drop it.
						//
						// Read ONCE, compared against the FULL captured address, and
						// every emission below uses the CAPTURED values — never a
						// re-read. The check and the emit are adjacent and
						// synchronous; deferring either behind a timer or a microtask
						// would reopen the window this closes.
						//
						// WHAT THIS FENCE DOES NOT CATCH, stated because it is a real
						// gap and not an oversight: it compares VALUES, so an address
						// that leaves and RETURNS (A→B→A) reads as unchanged. That is
						// reachable — the pane's `ItemDetail` has no `{#key}` (PLAN-2105
						// / TASK-2112), so an A→B→A item switch keeps one host token,
						// and the comment composer it owns is reused across it.
						//
						// It is left as-is deliberately, on two grounds. First, the
						// outcome differs from what the fence exists to prevent: the
						// user has NOT left the pane (they are back on it), and the
						// gesture is NOT re-attributed (same node, same attachment,
						// same host), so what opens is the image they clicked over the
						// pane they are looking at. Compare the uuid A→B→A case just
						// below, which IS fenced by `activationSeq` — there the
						// SUBJECT of the gesture changes, which is a different and
						// worse thing than a destination that round-trips to itself.
						//
						// Second, it is not fixable from inside this NodeView. Telling
						// A→B→A from A needs a generation that advances on every
						// address CHANGE, and this NodeView cannot observe one: it
						// only READS the address (`AttachmentHostAddressReader` is a
						// getter, with no subscription and no epoch), so a B that
						// arrives and leaves between the two reads is invisible here
						// by construction. Closing it means adding an epoch to
						// `AttachmentHostAddress` and bumping it in every host — a
						// cross-surface API change that also lands on the chip
						// NodeView, which is outside this task's contract. Tracked as
						// a fence-completeness follow-up rather than smuggled in here.
						const to = opts.address();
						if (
							to.workspaceSlug !== fromWs ||
							to.itemId !== fromItem ||
							to.hostToken !== fromHost
						) {
							return;
						}

						// ONE surface emit (T4a, TASK-2489): the raster/non-raster fork is
						// gone. The converged surface opens ANY attachment — a raster
						// image on its <img> arm, an `image/svg+xml` (active content, so
						// never rendered as bytes) or any other type on the no-bytes
						// fallback arm — so admission no longer branches on the MIME here;
						// the surface's own `getSurfaceRenderer` picks the arm. The
						// resolve-before-emit gate above (missing → latch, transient →
						// retry) is unchanged: this only runs on a resolved probe.
						//
						// `filename` is null: the node's attrs carry none and the HEAD
						// does not either — the same absence `applyImageSemantics` names
						// its fallback for. `width`/`height` likewise (3b's pixel policy
						// wants them; the HEAD has no source). `workspaceSlug` is the one
						// captured at activation (`fromWs`), never read live — the pane
						// can switch workspace under an open surface.
						notifyAttachmentSurfaceOpen({
							attachmentId: forUuid,
							workspaceSlug: fromWs,
							itemId: fromItem,
							hostToken: fromHost,
							// A single-image set: this NodeView knows about ITS node and
							// nothing else. The body's other images are a set the editor
							// could offer, but assembling one here would make ←/→ page
							// through a list this surface does not own.
							images: [
								{
									id: forUuid,
									alt: currentAlt,
									filename: null,
									mime_type: result.mime,
									size_bytes: result.size,
									width: null,
									height: null,
								},
							],
							index: 0,
							// Where the surface aims focus on close. The <img> is a real
							// focus stop as of TASK-2432, so this is a stable target rather
							// than whatever held focus at open (the surface still falls
							// back to the body if the element became unfocusable/detached).
							invoker: img,
							// Single-open seeds describing images[0].
							filename: null,
							mime_type: result.mime,
							size_bytes: result.size,
						});
					})
					.finally(() => {
						// Only if this activation is still the one holding the latch.
						// A uuid swap bumps the counter, so a stale resolution
						// landing afterwards cannot unlock the request that
						// replaced it — nor clear a pending state the request that
						// replaced it is still entitled to show.
						if (activationSeq !== seq) return;
						activating = false;
						// The DOM write, unlike the latch release, is subject to
						// this file's teardown rule: `destroyed` means every async
						// continuation stops touching the node. Harmless in
						// practice — the element is detached — but the `.then()`
						// above fences on it and a finalizer that does not is the
						// kind of asymmetry that stops being harmless the first
						// time someone puts something real in here.
						if (destroyed) return;
						setActivationPending(false);
					});
			}

			img.addEventListener('click', (event) => {
				// In a contenteditable, ProseMirror handles selection on
				// mousedown; intercept click so a single click opens the
				// lightbox without being swallowed as "click into the
				// editor selection". Multi-click events (double-click, etc.)
				// fall through so users can still drag-select around the
				// image without triggering the modal.
				if (event.detail > 1) return;
				event.preventDefault();
				event.stopPropagation();
				activate();
			});

			img.addEventListener('keydown', (event) => {
				const isSpace = event.key === ' ' || event.key === 'Spacebar';
				if (event.key !== 'Enter' && !isSpace) return;
				// A modified key is a SHORTCUT, not an activation — the same guard
				// the file chip next door carries (attachment-chip.ts). Without it
				// this handler swallows Ctrl/Cmd+Enter, which is CommentEditor's
				// submit binding (CommentEditor.svelte's handleKeyDown): focus an
				// image in a comment, press Cmd+Enter, and the comment does not
				// post — it opens a viewer instead.
				if (event.ctrlKey || event.metaKey || event.altKey || event.shiftKey) return;
				// Space would otherwise scroll the page, and inside a
				// contenteditable both keys would additionally be taken as text
				// input against the selected atom.
				event.preventDefault();
				// UNCONDITIONAL, and before activate(): ItemTimeline delegates its
				// own thumbnail click/keydown handlers across the whole entry list,
				// and that list CONTAINS live CommentEditor instances whose bodies
				// render this very NodeView — an `img[data-attachment-id]`, which is
				// exactly the selector that delegation matches. Without this, one
				// keypress opens two viewers.
				//
				// Note this is the ONLY activation the key produces: the browser
				// synthesizes a click from Enter/Space for real <button>s, not for
				// an <img role="button">, so there is no second fire to suppress.
				event.stopPropagation();
				// A HELD key repeats, and every repeat is another keydown — leaning
				// on Enter would stack one viewer per repeat. "Fires exactly once"
				// has to mean once per GESTURE, not once per event.
				//
				// Suppressed above rather than below: the repeat still belongs to
				// this activation, so letting it escape would hand the surrounding
				// surface a key the user only pressed once.
				if (event.repeat) return;
				activate();
			});

			// Build the toolbar lazily — only when the node is first
			// selected — so non-selected images don't carry the DOM
			// cost. Subsequent selections reuse the same toolbar.
			let toolbar: HTMLElement | null = null;
			let unregisterRefresher: (() => void) | null = null;
			const refresh = () => {
				if (!toolbar) return;
				// A confirmed deletion inertizes the WHOLE node, not just its
				// placeholder: rotate and crop against a row that is gone can
				// only 404, and leaving them live is the same dead-control gap
				// the placeholder's role/tabindex removal closes (round 7).
				if (deleted) {
					toolbar
						.querySelectorAll<HTMLButtonElement>('.attachment-image-toolbar-btn')
						.forEach((btn) => {
							btn.disabled = true;
							btn.title = 'This attachment has been deleted';
						});
					return;
				}
				// Read through the option, never a captured list: `opts` is a
				// snapshot of `this.options` taken when this NodeView was built,
				// which for a toolbar that predates the capabilities fetch is
				// before the server's formats are known (BUG-2426).
				refreshToolbarState(toolbar, knownMime, opts.supportedFormats());
			};
			const ensureToolbar = (): HTMLElement => {
				if (toolbar) return toolbar;
				toolbar = buildRotateToolbar({
					onRotate: (degrees) => runRotate(degrees),
					onCrop: () => runCrop(),
				});
				wrapper.appendChild(toolbar);
				// Subscribe to capability-change notifications. The
				// editor pushes the supportedFormats list async after
				// /server/capabilities resolves; without this hook a
				// toolbar created before that arrived would be stuck
				// in "no processor" state until the NodeView was
				// destroyed. Disposed in destroy() so the registry
				// doesn't accumulate leaked refs.
				unregisterRefresher = registerToolbarRefresher(refresh);
				// Probe the image's MIME so we can refine the toolbar's
				// per-format gating. Empty workspaceSlug = no probe
				// (e.g. SSR / preview surfaces) — the toolbar's state
				// falls back to the supportedFormats list alone, with
				// the MIME left null.
				const toolbarProbeWs = opts.address().workspaceSlug;
				if (currentUuid && toolbarProbeWs) {
					const probeUuid = currentUuid;
					const seqAtStart = stateSeq;
					fetchAttachmentMetadata(toolbarProbeWs, probeUuid, opts.getDownloadUrl).then(
						(result) => {
							// Bail if the NodeView was torn down, or if its uuid
							// changed (rotate/peer op) while the probe was in
							// flight — otherwise we'd touch detached DOM, or
							// cache stale MIME state for the new image.
							if (destroyed) return;
							if (currentUuid !== probeUuid) return;
							// A 404 here is the same authoritative signal the
							// load path acts on (DR-17), and this probe may
							// well beat the <img> to it.
							if (result.status === 'missing') latchMissing(probeUuid, seqAtStart);
							// `transient` leaves the MIME unknown rather than
							// wrong: gating falls back to supportedFormats.
							knownMime = result.status === 'ok' ? result.mime : null;
							// A resolved MIME can turn this image from
							// viewable-by-assumption into refused, which has to take
							// the button semantics back off. Applied BEFORE the
							// toolbar bail-out: the semantics are a property of the
							// image, not of whether a toolbar happens to exist.
							applyImageSemantics();
							if (!toolbar) return;
							refresh();
						}
					);
				}
				refresh();
				return toolbar;
			};

			const swapNodeUuid = (newId: string): void => {
				// A transform that resolves after teardown has no position left
				// to dispatch against — `editor.isDestroyed` is false whenever
				// the editor outlives this one NodeView, which is the common
				// case (the node was replaced, the doc was re-rendered).
				if (destroyed) return;
				// Master-freeze / R12 (TASK-2172): runRotate/runCrop gate editability
				// at CLICK time, but the transform awaits a network round-trip during
				// which the master can begin peeking — flipping the editor read-only
				// (PLAN-2179 DR-1 / TASK-2180: `peeking` no longer remounts via the
				// `{#key}`; it flips `editable=false` on the SAME view) — or a genuine
				// remount (item switch / forceRefreshNonce) can destroy THIS NodeView's
				// editor. Re-check right before the dispatch: a destroyed view would
				// throw, and a read-only one must not receive the Yjs transaction the
				// freeze forbids. The server-side transform still ran; only its doc
				// reference is dropped (no crash, no committed-content loss).
				if (editor.isDestroyed || !editor.isEditable) return;
				const pos = typeof getPos === 'function' ? getPos() : null;
				if (pos == null) return;
				// Replace the node's UUID at its current position.
				// setNodeMarkup keeps the same node type and only
				// rewrites attributes, which is what we want — the
				// transform produced a NEW attachment row, but it's
				// still an attachmentImage. The dispatch fires update()
				// synchronously, which handles old-UUID metadata
				// invalidation + img.src refresh (any source: local
				// rotate, peer Yjs op, ...).
				//
				// Critical: read attrs from the live document, NOT from
				// the `node` captured at NodeView creation. Now that the
				// NodeView survives attr updates, the closure-captured
				// `node` is stale — using it would clobber any concurrent
				// edits to other attrs (e.g. a peer's alt-text change
				// landing between two local rotates).
				const liveNode = editor.state.doc.nodeAt(pos);
				if (!liveNode) return;
				const tr = editor.state.tr.setNodeMarkup(pos, undefined, {
					...liveNode.attrs,
					uuid: newId,
				});
				editor.view.dispatch(tr);
			};

			const runRotate = async (degrees: 90 | 180 | 270): Promise<void> => {
				// Master-freeze / R12 (PLAN-2154 / TASK-2172): tiptap's `editable`
				// flag does NOT gate this NodeView toolbar, so a read-only editor
				// (e.g. a peeking full-page master) could otherwise rotate the
				// attachment and dispatch a Yjs transaction. Refuse the transform
				// unless the editor is currently editable.
				if (!editor.isEditable) return;
				// Snapshot uuid at click time. Now that update() keeps the
				// NodeView alive, currentUuid can shift while transform is
				// in flight (e.g. peer rotates the same image). Without the
				// snapshot we'd swap based on the OLD-uuid's transform
				// result while the live node already points at a different
				// image.
				const startUuid = currentUuid;
				if (!startUuid) return;
				try {
					const result = await opts.transform(startUuid, { operation: 'rotate', degrees });
					// If currentUuid drifted while transform was awaiting,
					// our result corresponds to a stale base — discard it.
					if (currentUuid !== startUuid) return;
					swapNodeUuid(result.id);
				} catch (err) {
					// Same fence as the success path: a failure that lands after
					// teardown, or after the node moved to a different image,
					// belongs to work the user has already navigated away from.
					// Reporting it would alert about attachment A while they are
					// looking at B.
					if (destroyed || currentUuid !== startUuid) return;
					const msg = err instanceof Error ? err.message : 'Rotation failed';
					if (opts.onError) opts.onError(msg);
					else if (typeof console !== 'undefined') console.error('[attachmentImage] rotate', err);
				}
			};

			const runCrop = async (): Promise<void> => {
				// Master-freeze / R12 (PLAN-2154 / TASK-2172): same editable gate as
				// runRotate — a read-only editor must not open the crop modal or
				// dispatch the resulting transform.
				if (!editor.isEditable) return;
				// Snapshot uuid + alt at click time. The crop rect the user
				// chooses is bound to the IMAGE-AT-OPEN-TIME, so any drift
				// (peer rotated/cropped while the modal is open) must
				// invalidate the result rather than apply it to a different
				// image. Same hazard as runRotate but compounded by the
				// modal's longer await window.
				const startUuid = currentUuid;
				const startAlt = currentAlt;
				if (!startUuid) return;
				// Open the crop modal pointing at the original-resolution
				// variant — the user is composing pixel-precise rect
				// coordinates, so we can't show the thumb-md downscale.
				// The modal returns rect coordinates already translated
				// to natural-image pixel space, ready to send to the
				// server unchanged.
				const fullUrl = opts.getDownloadUrl(startUuid, 'original');
				let rect: CropResult | null = null;
				try {
					rect = await openCropModal({ imageUrl: fullUrl, alt: startAlt });
				} catch {
					// openCropModal never rejects today, but defensive
					// catch keeps the editor responsive if that contract
					// changes — treat as cancel.
					rect = null;
				}
				if (rect == null) return;
				// Master-freeze / R14 (TASK-2172); tracked edge BUG-2177: peeking can
				// begin WHILE the crop modal is open. Re-check editability after it
				// resolves — before the server-side transform — so a frozen/remounted
				// master starts no transform (a crop initiated pre-pane whose editor
				// remounts is the accepted BUG-2177 orphan: no crash, no content loss).
				if (editor.isDestroyed || !editor.isEditable) return;
				// Live node moved to a different uuid while the modal was
				// open — the rect doesn't apply. Drop silently rather than
				// mis-cropping the new image.
				if (currentUuid !== startUuid) return;
				try {
					const result = await opts.transform(startUuid, { operation: 'crop', rect });
					if (currentUuid !== startUuid) return;
					swapNodeUuid(result.id);
				} catch (err) {
					// Same fence as the success path: a failure that lands after
					// teardown, or after the node moved to a different image,
					// belongs to work the user has already navigated away from.
					// Reporting it would alert about attachment A while they are
					// looking at B.
					if (destroyed || currentUuid !== startUuid) return;
					const msg = err instanceof Error ? err.message : 'Crop failed';
					if (opts.onError) opts.onError(msg);
					else if (typeof console !== 'undefined') console.error('[attachmentImage] crop', err);
				}
			};

			wrapper.appendChild(img);
			wrapper.appendChild(missing);
			return {
				dom: wrapper,
				// Refresh the live <img> in place when attrs change. Without
				// this, ProseMirror destroys + recreates the NodeView on any
				// attr change — fine for single-user rotate (the destroy was
				// invisible), but a hard regression once Yjs collab lands
				// (a remote peer's rotate would jump the local cursor + flicker
				// the image). Returning true keeps the NodeView alive across
				// in-place updates; returning false forces recreate when the
				// node type itself changed (shouldn't normally happen for an
				// attachmentImage, but covered defensively).
				update(updatedNode: ProseMirrorNode) {
					if (updatedNode.type.name !== node.type.name) return false;

					const newUuid = (updatedNode.attrs.uuid as string | null) ?? '';
					const newAlt = (updatedNode.attrs.alt as string | null) ?? '';

					if (newUuid !== currentUuid) {
						// Drop cached metadata for the OLD uuid — next probe
						// (e.g. on the same image rendered elsewhere) will
						// re-fetch. Triggers for any source of the change:
						// local rotate via swapNodeUuid OR a peer Yjs op.
						const swapWs = opts.address().workspaceSlug;
						if (currentUuid && swapWs) {
							invalidateAttachmentMetadata(swapWs, currentUuid);
						}
						currentUuid = newUuid;
						// The old uuid's state — whether a 404 placeholder or a
						// confirmed deletion — says nothing about the new one.
						deleted = false;
						// And every continuation still resolving for the OLD target is
						// now stale. Comparing `currentUuid` alone would let a swap AWAY
						// AND BACK pass the check again (BUG-2509 / Codex round 2).
						stateSeq += 1;
						// Nor does an activation still resolving for it. That
						// request is already fenced (its continuation compares
						// `currentUuid`), so the latch is guarding nothing but the
						// NEW image — and this NodeView deliberately outlives the
						// swap, so a HEAD that never settles would leave the image
						// in front of the user permanently unopenable.
						//
						// The bump is what retires any request still resolving for
						// the old attachment: its continuation checks the generation
						// before emitting, so a swap AWAY AND BACK cannot let it
						// find its own uuid in place and open a viewer for a gesture
						// the user made two swaps ago.
						activationSeq += 1;
						activating = false;
						// And the pending affordance goes with the latch: the bump
						// above retires the old request's finalizer, so leaving
						// `aria-busy` on would strand the NEW image as permanently
						// busy.
						setActivationPending(false);
						resetMissing();
						if (newUuid) {
							loadImage(opts.getDownloadUrl(newUuid, 'thumb-md'));
							img.setAttribute('data-attachment-id', newUuid);
						} else {
							// Detach explicitly: this branch clears the src without
							// going through loadImage(), so the previous load's
							// listeners would survive and a queued error could show
							// the placeholder on a now-empty node (Codex round 8).
							detachLoadListeners();
							img.removeAttribute('src');
							img.removeAttribute('data-attachment-id');
						}
						// Toolbar's MIME probe was for the old uuid; reset
						// gating to "still loading" + re-probe so per-format
						// state stays correct across the swap.
						knownMime = null;
						refresh();
						// The new uuid is unprobed, so the image is activatable
						// again on the same "unknown ⇒ today's behaviour" rule the
						// gate uses; a stale refusal must not outlive the image it
						// was about.
						applyImageSemantics();
						const updateProbeWs = opts.address().workspaceSlug;
						if (toolbar && newUuid && updateProbeWs) {
							const probeUuid = newUuid;
							// Captured AFTER this swap's own bump, so the fence retires
							// probes from before the swap, not this one.
							const seqAtStart = stateSeq;
							fetchAttachmentMetadata(
								updateProbeWs,
								probeUuid,
								opts.getDownloadUrl
							).then((result) => {
								if (destroyed) return;
								if (currentUuid !== probeUuid) return;
								if (result.status === 'missing') latchMissing(probeUuid, seqAtStart);
								knownMime = result.status === 'ok' ? result.mime : null;
								applyImageSemantics();
								if (!toolbar) return;
								refresh();
							});
						}
					}

					if (newAlt !== currentAlt) {
						currentAlt = newAlt;
						if (newAlt) img.alt = newAlt;
						else img.removeAttribute('alt');
						// The accessible name is derived from alt, so it has to
						// follow a peer's (or a local) alt edit rather than keep
						// announcing the old text.
						applyImageSemantics();
					}

					return true;
				},
				selectNode() {
					wrapper.classList.add('attachment-image-selected');
					// Master-freeze / R12 (TASK-2172): don't surface the rotate/crop
					// toolbar on a read-only editor — a peeking master OR a view-only
					// viewer. Its buttons no-op via the editable gate in
					// runRotate/runCrop; hiding it keeps the freeze visually honest.
					// Closing this for viewers too is a bug fix (their transforms had
					// no provider to sync and 403'd on save), not a working-flow change.
					if (!editor.isEditable) return;
					const tb = ensureToolbar();
					tb.classList.remove('attachment-image-toolbar-hidden');
				},
				deselectNode() {
					wrapper.classList.remove('attachment-image-selected');
					if (toolbar) toolbar.classList.add('attachment-image-toolbar-hidden');
				},
				destroy() {
					// Set FIRST: every async continuation below fences on it, and
					// a HEAD probe in flight at teardown would otherwise resolve
					// into detached DOM (the chip NodeView has carried this flag
					// since it was written; the image one did not).
					destroyed = true;
					detachLoadListeners();
					disposeDeletionListener();
					disposeRestoreListener();
					// Tear down the refresher subscription so the
					// module-level registry doesn't pile up stale
					// callbacks across editor lifecycles (e.g. SPA
					// navigation between item views).
					if (unregisterRefresher) {
						unregisterRefresher();
						unregisterRefresher = null;
					}
				},
			};
		};
	},

	addStorage() {
		return {
			markdown: {
				/**
				 * Emit `![alt](pad-attachment:UUID)`. tiptap-markdown's serializer
				 * expects this signature — the `state` object exposes `write`,
				 * `closeBlock`, etc. We only need `write` here since the node is
				 * inline.
				 */
				serialize(state: { write: (s: string) => void }, node: { attrs: { uuid: unknown; alt: unknown } }) {
					const uuid = node.attrs.uuid;
					if (typeof uuid !== 'string' || uuid === '') return;
					const altRaw = typeof node.attrs.alt === 'string' ? node.attrs.alt : '';
					state.write(`![${escapeMarkdownAlt(altRaw)}](${PAD_ATTACHMENT_PREFIX}${uuid})`);
				},
				parse: {
					// markdown-it's default image token already produces
					// <img src="pad-attachment:UUID" alt="…">; our parseHTML
					// rules pick that up. No custom markdown-it rule needed.
				},
			},
		};
	},

	addCommands() {
		return {
			setAttachmentImage:
				(options) =>
				({ commands }) =>
					commands.insertContent({
						type: this.name,
						attrs: { uuid: options.uuid, alt: options.alt ?? null },
					}),
		};
	},
});

/**
 * Build the image-edit toolbar — rotate trio (left 90°, 180°, right
 * 90°) plus a crop button. Each button delegates to the supplied
 * callback; the NodeView wires those into setNodeMarkup transactions
 * once the server returns the new UUID.
 *
 * The toolbar starts in its enabled state. refreshToolbarState
 * disables individual buttons (with tooltip) once the image's MIME
 * has been probed and compared to the processor's supported formats
 * list — every button gates on the same per-format check, since
 * both rotate and crop go through the same /transform path.
 */
function buildRotateToolbar(opts: {
	onRotate: (degrees: 90 | 180 | 270) => void;
	onCrop: () => void;
}): HTMLElement {
	const toolbar = document.createElement('div');
	toolbar.className = 'attachment-image-toolbar';
	toolbar.setAttribute('contenteditable', 'false');
	// Stop mousedown from racing the editor's click-to-select handler
	// — without this, clicking a button steals selection focus from the
	// node and the subsequent setNodeMarkup transaction lands on the
	// wrong target. Same trick as the code-block "Copy" button in
	// Editor.svelte.
	toolbar.addEventListener('mousedown', (e) => e.preventDefault());

	const rotateButton = (label: string, title: string, deg: 90 | 180 | 270): HTMLButtonElement => {
		const btn = document.createElement('button');
		btn.type = 'button';
		btn.className = 'attachment-image-toolbar-btn';
		btn.textContent = label;
		btn.title = title;
		btn.dataset.degrees = String(deg);
		btn.addEventListener('click', (e) => {
			e.preventDefault();
			e.stopPropagation();
			if (btn.disabled) return;
			opts.onRotate(deg);
		});
		return btn;
	};

	const cropButton = (): HTMLButtonElement => {
		const btn = document.createElement('button');
		btn.type = 'button';
		btn.className = 'attachment-image-toolbar-btn';
		btn.textContent = '⌶';
		btn.title = 'Crop…';
		btn.dataset.action = 'crop';
		btn.addEventListener('click', (e) => {
			e.preventDefault();
			e.stopPropagation();
			if (btn.disabled) return;
			opts.onCrop();
		});
		return btn;
	};

	toolbar.append(
		rotateButton('↶', 'Rotate left 90°', 270),
		rotateButton('↻', 'Rotate 180°', 180),
		rotateButton('↷', 'Rotate right 90°', 90),
		cropButton()
	);
	return toolbar;
}

/**
 * Update each toolbar button's enabled state + tooltip based on the
 * image's MIME and the processor's supported-formats list.
 *
 *   - Empty supportedFormats → all buttons disabled with a "no image
 *     processor on this build" message. Covers the libvips-tagged
 *     binary that hasn't shipped Phase 2 yet, plus self-hosters who
 *     opted out of image processing.
 *   - MIME unknown (still loading the HEAD probe, or the probe
 *     failed) → keep buttons enabled. Server returns 415 if the
 *     format actually isn't supported, and the editor's onError
 *     surfaces the message inline at click time.
 *   - MIME known and unsupported → disable with format-specific
 *     tooltip ("Image editing for image/webp requires libvips").
 *   - MIME known and supported → enabled with the original tooltip.
 */
function refreshToolbarState(
	toolbar: HTMLElement,
	mime: string | null,
	supportedFormats: string[]
): void {
	const btns = toolbar.querySelectorAll<HTMLButtonElement>('.attachment-image-toolbar-btn');
	const noProcessor = supportedFormats.length === 0;
	const format = mime ? mimeToFormat(mime) : null;
	// A KNOWN mime that maps to no processor format is unsupported, not
	// unknown. `mimeToFormat` returns null both for "never asked" and for
	// "asked, and this is not something the processor handles" — treating the
	// second as the first left Crop enabled for e.g. image/svg+xml, which
	// hands the original to the crop modal for a transform the server will
	// refuse (final review round 7).
	const knownUnsupported = !!mime && (!format || !supportedFormats.includes(format));

	btns.forEach((btn) => {
		// Re-derive the original tooltip from the dataset. We keep the
		// canonical title in the data-original-title attribute so we
		// can restore it after the disabled state clears.
		const originalTitle =
			btn.dataset.originalTitle ?? btn.title ?? '';
		btn.dataset.originalTitle = originalTitle;

		if (noProcessor) {
			btn.disabled = true;
			btn.title = 'Image editing not available in this build (libvips backend not shipped yet)';
			return;
		}
		if (knownUnsupported) {
			btn.disabled = true;
			btn.title = format
				? `Image editing for ${mime} requires libvips (this build supports ${supportedFormats.join(', ')})`
				: `Image editing isn't available for ${mime}`;
			return;
		}
		btn.disabled = false;
		btn.title = originalTitle;
	});
}

/*
 * THERE IS NO LIGHTBOX IN THIS FILE, AND THERE MUST NOT BE ONE AGAIN.
 *
 * `openImageLightbox` / `closeLightbox` used to live here: a hand-rolled
 * `<dialog>` this NodeView appended to `document.body` and `showModal()`d.
 * TASK-2433 deleted them. Inline images now emit on the unified SURFACE channel
 * (`notifyAttachmentSurfaceOpen`, in activate() above — T4a) and the ONE
 * `AttachmentSurfaceHost` that `ItemDetail` owns mounts the `Lightbox` for this
 * route AND every other producer — which is where the modal contract lives: the
 * lease-stacked backdrop, the focus trap and restore, the Escape ordering, and
 * the DR-16 gate re-applied over the whole set. (The strip and the timeline emit
 * on the same channel now too, since T4a — no producer mounts `Lightbox` itself.)
 *
 * A NodeView cannot mount a Svelte component, which is the entire reason the
 * bus exists. Re-adding an imperative overlay here would not be a shortcut, it
 * would be a second viewer with none of that contract — so
 * `attachmentImageNoOverlay.test.ts` asserts, statically, that this file
 * creates no dialog and calls no `showModal`.
 */
