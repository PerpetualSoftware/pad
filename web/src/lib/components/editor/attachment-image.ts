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

/** Variants the download URL builder must support. Mirrors the API. */
export type AttachmentVariant = 'thumb-sm' | 'thumb-md' | 'original';

/** URL builder injected by Editor.svelte at configure time. */
export type AttachmentUrlBuilder = (uuid: string, variant?: AttachmentVariant) => string;

export interface AttachmentImageOptions {
	HTMLAttributes: Record<string, unknown>;
	/**
	 * Resolves an attachment UUID to a download URL. Default implementation
	 * returns the literal `pad-attachment:UUID` reference — sufficient for
	 * markdown round-trip, but the editor will configure it to the actual
	 * `/api/v1/workspaces/{ws}/attachments/{id}` endpoint so images render.
	 */
	getDownloadUrl: AttachmentUrlBuilder;
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
	 * NodeView attaches a click handler that opens the original-resolution
	 * variant in a lightbox dialog. Tiptap calls renderHTML for clipboard
	 * / `getHTML()` / serialization paths, so both must stay in sync — the
	 * NodeView is only the live editor representation.
	 */
	addNodeView() {
		return ({ node }) => {
			const img = document.createElement('img');
			img.classList.add('attachment-image');
			img.loading = 'lazy';
			const uuid = (node.attrs.uuid as string | null) ?? '';
			const alt = (node.attrs.alt as string | null) ?? '';
			if (uuid) {
				img.src = this.options.getDownloadUrl(uuid, 'thumb-md');
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
				const fullUrl = this.options.getDownloadUrl(uuid, 'original');
				openImageLightbox(fullUrl, alt);
			});

			return { dom: img };
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
