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
import {
	type AttachmentUrlBuilder,
	type AttachmentVariant,
	fetchAttachmentMetadata,
	invalidateAttachmentMetadata,
	mimeToFormat
} from './attachment-metadata';
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
	 * Workspace slug used for HEAD-probing the image's MIME (so the
	 * rotate toolbar can gate buttons on the processor's supported
	 * formats). Empty string disables the probe — the toolbar still
	 * shows but skips per-format gating.
	 */
	workspaceSlug: string;
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
			const uuid = (node.attrs.uuid as string | null) ?? '';
			const alt = (node.attrs.alt as string | null) ?? '';
			if (uuid) {
				img.src = opts.getDownloadUrl(uuid, 'thumb-md');
				img.setAttribute('data-attachment-id', uuid);
			}
			if (alt) img.alt = alt;

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
				if (!uuid) return;
				const fullUrl = opts.getDownloadUrl(uuid, 'original');
				openImageLightbox(fullUrl, alt);
			});

			// Build the toolbar lazily — only when the node is first
			// selected — so non-selected images don't carry the DOM
			// cost. Subsequent selections reuse the same toolbar.
			let toolbar: HTMLElement | null = null;
			let toolbarMime: string | null = null;
			let unregisterRefresher: (() => void) | null = null;
			const refresh = () => {
				if (toolbar) refreshToolbarState(toolbar, toolbarMime, opts.supportedFormats);
			};
			const ensureToolbar = (): HTMLElement => {
				if (toolbar) return toolbar;
				toolbar = buildRotateToolbar({
					onRotate: (degrees) => runRotate(degrees),
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
				if (uuid && opts.workspaceSlug) {
					fetchAttachmentMetadata(opts.workspaceSlug, uuid, opts.getDownloadUrl).then(
						(meta) => {
							if (!toolbar) return;
							toolbarMime = meta?.mime ?? null;
							refresh();
						}
					);
				}
				refresh();
				return toolbar;
			};

			const runRotate = async (degrees: 90 | 180 | 270): Promise<void> => {
				if (!uuid) return;
				try {
					const result = await opts.transform(uuid, { operation: 'rotate', degrees });
					const pos = typeof getPos === 'function' ? getPos() : null;
					if (pos == null) return;
					// Replace the node's UUID at its current position.
					// setNodeMarkup keeps the same node type and only
					// rewrites attributes, which is what we want — the
					// transform produced a NEW attachment row, but it's
					// still an attachmentImage.
					const tr = editor.state.tr.setNodeMarkup(pos, undefined, {
						...node.attrs,
						uuid: result.id,
					});
					editor.view.dispatch(tr);
					// Drop the cached metadata for the OLD UUID — the
					// editor may render the original elsewhere later
					// and we want the next probe to refresh.
					if (opts.workspaceSlug) {
						invalidateAttachmentMetadata(opts.workspaceSlug, uuid);
					}
				} catch (err) {
					const msg = err instanceof Error ? err.message : 'Rotation failed';
					if (opts.onError) opts.onError(msg);
					else if (typeof console !== 'undefined') console.error('[attachmentImage] rotate', err);
				}
			};

			wrapper.appendChild(img);
			return {
				dom: wrapper,
				selectNode() {
					wrapper.classList.add('attachment-image-selected');
					const tb = ensureToolbar();
					tb.classList.remove('attachment-image-toolbar-hidden');
				},
				deselectNode() {
					wrapper.classList.remove('attachment-image-selected');
					if (toolbar) toolbar.classList.add('attachment-image-toolbar-hidden');
				},
				destroy() {
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
 * Build the rotate toolbar — three buttons (rotate left 90°,
 * rotate 180°, rotate right 90°). Each button delegates to the
 * supplied onRotate callback; the NodeView wires that into a
 * setNodeMarkup transaction once the server returns the new UUID.
 *
 * The toolbar starts in its enabled state. refreshToolbarState
 * disables individual buttons (with tooltip) once the image's
 * MIME has been probed and compared to the processor's supported
 * formats list.
 */
function buildRotateToolbar(opts: {
	onRotate: (degrees: 90 | 180 | 270) => void;
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

	const button = (label: string, title: string, deg: 90 | 180 | 270): HTMLButtonElement => {
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

	toolbar.append(
		button('↶', 'Rotate left 90°', 270),
		button('↻', 'Rotate 180°', 180),
		button('↷', 'Rotate right 90°', 90)
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
	const knownUnsupported = !!format && !supportedFormats.includes(format);

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
			btn.title = `Image editing for ${mime} requires libvips (this build supports ${supportedFormats.join(', ')})`;
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
