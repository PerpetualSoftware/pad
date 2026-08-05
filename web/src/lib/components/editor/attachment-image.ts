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
import { registerAttachmentDeletionListener } from '$lib/attachments/events';
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
 * after `api.server.capabilities()` resolves and the new
 * `supportedFormats` list has been pushed onto the AttachmentImage
 * extension's options. Any toolbar that was sitting in the
 * "all-disabled" pre-capabilities state snaps to its correct
 * per-format gating.
 */
export function notifyAttachmentImageCapabilitiesChanged(): void {
	for (const fn of toolbarRefreshers) fn();
}

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
	 * panel / viewer events (PLAN-2392 DR-8). A reader rather than two
	 * strings — see `$lib/attachments/hostAddress` for why writing options
	 * after configure cannot work.
	 */
	address: AttachmentHostAddressReader;
	/**
	 * Image formats the server-side processor supports. Drives the
	 * rotate toolbar's enabled state per attachment: a button is
	 * disabled (with tooltip) when the image's MIME isn't in this
	 * list. Empty list ⇒ all transforms disabled (degraded build).
	 */
	supportedFormats: string[];
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
			supportedFormats: [] as string[],
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
			 *   - a KNOWN MIME outside the viewer allowlist — activate() refuses it
			 *     (DR-16), so the same dead stop applies. An UNPROBED node keeps the
			 *     semantics: "not yet asked" is not "not viewable", and the probe is
			 *     lazy. This mirrors ItemTimeline.svelte's semantics pass, which is
			 *     likewise able to take the semantics back OFF when a probe resolves.
			 *
			 *     Consequence, accepted deliberately: because the probe is lazy, a
			 *     refused image can be FOCUSED before its MIME resolves and lose
			 *     focus when it does. The alternative is leaving an SVG as a focus
			 *     stop that announces itself as a button and does nothing, which is
			 *     the failure this whole rule exists to prevent — and the same
			 *     trade the placeholder below already makes.
			 *
			 * The accessible name comes from alt with a GENERIC fallback: there is
			 * no filename on the node's attrs and the HEAD metadata does not carry
			 * one either, so the filename form DR-12 sketches has no source here.
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
			 */
			function canActivate(): boolean {
				return (
					!!currentUuid &&
					!deleted &&
					// Hidden means the placeholder has taken over: either a
					// confirmed deletion or a load failure. Neither has an image
					// to show, so neither has anything to open.
					img.style.display !== 'none' &&
					!(knownMime && !canOpenInViewer(knownMime))
				);
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
					currentAlt ? `View image: ${currentAlt}` : 'View attachment image'
				);
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
			function latchMissing(forUuid: string) {
				if (destroyed || deleted) return;
				if (!forUuid || currentUuid !== forUuid) return;
				deleted = true;
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
				void revalidateAttachmentMetadata(probeWs, forUuid, opts.getDownloadUrl).then(
					(result) => {
						if (result.status === 'missing') latchMissing(forUuid);
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

			const disposeDeletionListener = registerAttachmentDeletionListener((deletedUuid) => {
				if (deletedUuid !== currentUuid) return;
				deleted = true;
				// Drop the in-flight request's listeners: its `load` would
				// otherwise fire after the delete and restore the image.
				detachLoadListeners();
				showMissing();
				refresh();
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
			 * The MIME gate used to live inside the click handler, which made it a
			 * property of the MOUSE rather than of activation: a keyboard path that
			 * opened on its own would have bypassed it entirely. One function owns
			 * the gate so there is exactly one place a route can be added to and
			 * exactly one place the gate can be strengthened (the viewer bus and its
			 * fence land on this same seam in the tasks that follow).
			 *
			 * The gate itself is `canActivate()`, shared with the semantics pass so
			 * an image can never be openable and un-announced, or announced and
			 * dead.
			 *
			 * DR-16: the EXACT raster allowlist gates every open-the-viewer path,
			 * not just the strip's — `image/svg+xml` can carry active content, and a
			 * node being labelled image/* is not sufficient reason to hand it to a
			 * viewer.
			 *
			 * Gated on what is POSITIVELY KNOWN, deliberately: this node's MIME comes
			 * from a lazy HEAD probe, so at activation time it is often simply
			 * unasked. Refusing on unknown would stop ordinary images opening — a
			 * certain regression traded for a marginal risk — so an unprobed node
			 * keeps today's behaviour and a probed non-allowlisted one is refused.
			 */
			function activate(): void {
				if (!canActivate()) return;
				const fullUrl = opts.getDownloadUrl(currentUuid, 'original');
				openImageLightbox(fullUrl, currentAlt);
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
				refreshToolbarState(toolbar, knownMime, opts.supportedFormats);
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
							if (result.status === 'missing') latchMissing(probeUuid);
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
							fetchAttachmentMetadata(
								updateProbeWs,
								probeUuid,
								opts.getDownloadUrl
							).then((result) => {
								if (destroyed) return;
								if (currentUuid !== probeUuid) return;
								if (result.status === 'missing') latchMissing(probeUuid);
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

/**
 * Open a centered <dialog> showing the full-resolution attachment.
 * Closes on backdrop click, the close button, or the Esc key.
 */
function openImageLightbox(fullUrl: string, alt: string): void {
	if (typeof document === 'undefined') return;
	const dialog = document.createElement('dialog');
	dialog.className = 'attachment-image-lightbox';

	const closeBtn = document.createElement('button');
	closeBtn.type = 'button';
	closeBtn.className = 'attachment-image-lightbox-close';
	closeBtn.setAttribute('aria-label', 'Close image preview');
	closeBtn.textContent = '×';
	closeBtn.addEventListener('click', () => closeLightbox(dialog));

	const img = document.createElement('img');
	img.className = 'attachment-image-lightbox-img';
	img.src = fullUrl;
	if (alt) img.alt = alt;
	// Prevent clicks on the image itself from bubbling to the backdrop
	// handler below (which closes the dialog).
	img.addEventListener('click', (event) => event.stopPropagation());

	dialog.append(closeBtn, img);
	dialog.addEventListener('click', () => closeLightbox(dialog));
	dialog.addEventListener('close', () => dialog.remove());

	document.body.appendChild(dialog);
	dialog.showModal();
}

function closeLightbox(dialog: HTMLDialogElement): void {
	if (dialog.open) dialog.close();
}
