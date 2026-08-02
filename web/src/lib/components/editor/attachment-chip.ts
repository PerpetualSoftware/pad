/**
 * AttachmentChip — Tiptap node for non-image `pad-attachment:UUID` references.
 *
 * Renders a Notion-style chip (icon + filename + size) for any attachment
 * the user wants surfaced as a downloadable file rather than embedded
 * inline. Stores the attachment UUID (not a backend URL) so item content
 * survives a storage-backend migration untouched. See DOC-865.
 *
 * Node shape:
 *   - `uuid`     : string — the attachments-row UUID (required)
 *   - `filename` : string — display name; preserved across save/reload
 *
 * Markdown round-trip:
 *   - Serialize: `[filename](pad-attachment:UUID)` via tiptap-markdown's
 *     addStorage.markdown.serialize. Standard link syntax — same form the
 *     read-only resolver in TASK-874 understands, so server-side and
 *     editor renders match.
 *   - Parse: markdown-it's link token produces `<a href="pad-attachment:UUID">filename</a>`,
 *     captured by parseHTML rule `a[href^="pad-attachment:"]` at priority
 *     1000 so it beats the SafeLink mark (default priority 50).
 *
 * Editor display: NodeView fires a single HEAD request per attachment
 * (cached module-globally) to read Content-Type and Content-Length from
 * the existing GET handler — no new API endpoint needed. The fetched
 * MIME upgrades the icon from the filename-extension guess to the
 * canonical file-type icon, and the size is rendered alongside the name.
 *
 * Icon and byte formatting are the shared attachment helpers'
 * (`$lib/attachments/display` + `$lib/attachments/icons`) as of TASK-2417 —
 * this file used to carry its own `iconForMime` / `iconForFilename` /
 * `formatBytes`. The one behavior that stayed local is hiding an unknown or
 * zero size: the shared formatter renders "0 B", and per PLAN-2392 DR-3b the
 * call site keeps that conditional rather than the helper growing a mode.
 */

import { Node, mergeAttributes } from '@tiptap/core';
import type { Node as ProseMirrorNode } from '@tiptap/pm/model';
import {
	type AttachmentUrlBuilder,
	type AttachmentVariant,
	fetchAttachmentMetadata
} from './attachment-metadata';
import { registerAttachmentDeletionListener } from '$lib/attachments/events';
import { formatBytes, iconForAttachment } from '$lib/attachments/display';
import { iconSvg } from '$lib/attachments/icons/index';

const PAD_ATTACHMENT_PREFIX = 'pad-attachment:';

// Re-export the shared types so existing chip-importing call sites
// keep working without having to change their imports.
export type { AttachmentVariant, AttachmentUrlBuilder };

export interface AttachmentChipOptions {
	HTMLAttributes: Record<string, unknown>;
	/** Build the download URL — usually `api.attachments.downloadUrl` from the editor's mount context. */
	getDownloadUrl: AttachmentUrlBuilder;
	/** Workspace slug used by the metadata HEAD fetcher. Empty disables the fetch. */
	workspaceSlug: string;
}

declare module '@tiptap/core' {
	interface Commands<ReturnType> {
		attachmentChip: {
			/**
			 * Insert a file chip at the current selection. Used by the
			 * upload plugin (TASK-875) on successful non-image uploads.
			 */
			setAttachmentChip: (options: { uuid: string; filename: string }) => ReturnType;
		};
	}
}

export const AttachmentChip = Node.create<AttachmentChipOptions>({
	name: 'attachmentChip',

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
		};
	},

	addAttributes() {
		return {
			uuid: {
				default: null,
				parseHTML: (element) => {
					const dataId = element.getAttribute('data-attachment-id');
					if (dataId) return dataId;
					const href = element.getAttribute('href') ?? '';
					if (href.startsWith(PAD_ATTACHMENT_PREFIX)) {
						return href.slice(PAD_ATTACHMENT_PREFIX.length);
					}
					return null;
				},
				renderHTML: (attrs) =>
					attrs.uuid ? { 'data-attachment-id': attrs.uuid } : {},
			},
			filename: {
				default: '',
				parseHTML: (element) => {
					// Prefer the explicit data attribute when present (editor
					// render output stamps it). For markdown-it output the
					// link's text content IS the filename — the round-trip
					// `[filename](pad-attachment:UUID)` puts it there.
					const attr = element.getAttribute('data-filename');
					if (attr) return attr;
					return element.textContent?.trim() ?? '';
				},
				renderHTML: (attrs) =>
					attrs.filename ? { 'data-filename': String(attrs.filename) } : {},
			},
		};
	},

	parseHTML() {
		return [
			// Priority 1000 beats SafeLink's default mark rule (50) so
			// `a[href^="pad-attachment:"]` becomes a chip Node, not a Link
			// Mark on plain text. The two coexist: regular `a[href=…]` still
			// matches SafeLink; only attachment refs are diverted here.
			{
				tag: 'a[href^="pad-attachment:"]',
				priority: 1000,
			},
			{
				tag: 'a[data-attachment-id]',
				priority: 1000,
			},
		];
	},

	renderHTML({ HTMLAttributes, node }) {
		const uuid = (node.attrs.uuid as string | null) ?? '';
		const filename = (node.attrs.filename as string | null) ?? '';
		const href = uuid ? this.options.getDownloadUrl(uuid) : '';
		// Static HTML output (used by clipboard / getHTML / SSR). The live
		// in-editor look is handled by the NodeView below — this fallback
		// just ensures pasted HTML survives a copy round-trip.
		return [
			'a',
			mergeAttributes(this.options.HTMLAttributes, HTMLAttributes, {
				class: 'file-chip',
				href,
				download: filename || true,
				target: '_blank',
				rel: 'noopener noreferrer',
			}),
			filename || 'attachment',
		];
	},

	addNodeView() {
		return ({ node }) => {
			// Mutable closure state — kept in sync via update() so attr
			// changes (peer Yjs ops once collab lands, future programmatic
			// rename/swap commands, etc.) refresh the live chip in place
			// instead of forcing ProseMirror to destroy and recreate the
			// NodeView. Same hazard pattern as BUG-1246 / TASK-1250.
			let currentUuid = (node.attrs.uuid as string | null) ?? '';
			let currentFilename = (node.attrs.filename as string | null) ?? '';
			let currentMime: string | null = null;

			const wrapper = document.createElement('a');
			wrapper.className = 'file-chip';
			wrapper.target = '_blank';
			wrapper.rel = 'noopener noreferrer';
			wrapper.contentEditable = 'false';

			const iconEl = document.createElement('span');
			iconEl.className = 'file-chip-icon';
			iconEl.setAttribute('aria-hidden', 'true');

			const nameEl = document.createElement('span');
			nameEl.className = 'file-chip-name';

			const sizeEl = document.createElement('span');
			sizeEl.className = 'file-chip-size';
			// Empty until metadata resolves; CSS hides empty separator span.

			wrapper.append(iconEl, nameEl, sizeEl);

			const refreshHref = (): void => {
				if (currentUuid) {
					wrapper.href = this.options.getDownloadUrl(currentUuid);
					wrapper.setAttribute('data-attachment-id', currentUuid);
				} else {
					wrapper.removeAttribute('href');
					wrapper.removeAttribute('data-attachment-id');
				}
			};

			const refreshFilenameDom = (): void => {
				if (currentFilename) {
					wrapper.setAttribute('data-filename', currentFilename);
					wrapper.download = currentFilename;
				} else {
					wrapper.removeAttribute('data-filename');
					wrapper.removeAttribute('download');
				}
				nameEl.textContent = currentFilename || 'attachment';
			};

			// Icon resolution is the shared mapper's (TASK-2417): MIME first,
			// filename-extension second, generic file last, so the chip is
			// never left iconless and never shows a "❓". `currentMime` is
			// null until the HEAD probe resolves, which is exactly the
			// filename-fallback case the mapper's second argument exists for.
			// innerHTML (not textContent) because the icons are SVG now;
			// iconSvg() interpolates only repo constants.
			const refreshIcon = (): void => {
				iconEl.innerHTML = iconSvg(iconForAttachment(currentMime, currentFilename));
			};

			/**
			 * A deleted attachment leaves this chip looking perfectly valid —
			 * unlike an <img>, a link makes no request until clicked, so
			 * nothing tells it the target is gone and the user gets a 404 in a
			 * new tab (Codex round 13). The strip broadcasts deletions, so mark
			 * the chip dead in place instead: same .attachment-missing
			 * treatment the markdown renderer uses for a missing reference.
			 *
			 * The dead state is carried by the .attachment-missing CLASS, not by
			 * the icon (TASK-2417). It used to swap the glyph to a paperclip,
			 * which the SVG set can't reproduce without inventing a "missing"
			 * icon that is a state rather than a format family — and a later
			 * filename update calls refreshIcon() and would quietly overwrite it
			 * anyway. app.css styles .file-chip.attachment-missing instead, so
			 * type and state are independent and neither clobbers the other.
			 */
			let deleted = false;
			const markDeleted = (): void => {
				deleted = true;
				wrapper.classList.add('attachment-missing');
				wrapper.removeAttribute('href');
				wrapper.removeAttribute('download');
				wrapper.title = 'This attachment has been deleted';
				sizeEl.textContent = '';
			};

			// Set by destroy(). A HEAD probe outlives the NodeView that started
			// it, and writing to detached DOM after teardown is at best wasted
			// work — same fence shape as the `forUuid` check below.
			let destroyed = false;

			const disposeDeletionListener = registerAttachmentDeletionListener((deletedUuid) => {
				if (deletedUuid === currentUuid) markDeleted();
			});

			refreshHref();
			refreshFilenameDom();
			refreshIcon();

			// Explicit click handler → window.open. Editor.svelte installs a
			// global anchor-click suppressor that calls preventDefault on
			// every <a> inside the editor (so plain text links don't navigate
			// in edit mode); without this handler the chip's anchor
			// navigation would also be eaten and clicking the chip would
			// silently do nothing. Mirrors the pattern AttachmentImage uses
			// for its lightbox click. Reads currentUuid (mutable) so a peer
			// Yjs op swapping the chip's target is honoured at click time.
			wrapper.addEventListener('click', (event) => {
				if (event.detail > 1) return; // double-click → fall through
				if (!currentUuid) return;
				// Removing href is not enough: this handler opens the URL
				// itself, so a deleted chip would still open a 404 in a new tab
				// (Codex round 14). Swallow the click instead.
				if (deleted) {
					event.preventDefault();
					event.stopPropagation();
					return;
				}
				event.preventDefault();
				event.stopPropagation();
				if (typeof window !== 'undefined') {
					window.open(
						this.options.getDownloadUrl(currentUuid),
						'_blank',
						'noopener,noreferrer',
					);
				}
			});

			// Async metadata enrichment via HEAD. Server registers HEAD
			// alongside GET (chi doesn't auto-route HEAD to GET handlers),
			// and the response carries Content-Type + Content-Length without
			// a body. The promise cache keyed by (workspace, uuid)
			// deduplicates repeated chips for the same attachment and
			// survives undo/redo. Skip the fetch when no workspace context
			// is available (e.g. headless rendering) — the chip still works,
			// just without size/icon refinement.
			//
			// `forUuid` is captured at call time so a probe in flight does
			// not trample state for a NEW uuid that landed via update()
			// while we were awaiting HEAD.
			const probeMetadata = (forUuid: string): void => {
				if (!forUuid || !this.options.workspaceSlug) return;
				fetchAttachmentMetadata(
					this.options.workspaceSlug,
					forUuid,
					this.options.getDownloadUrl,
				).then((meta) => {
					if (!meta) return;
					if (destroyed) return; // NodeView torn down while HEAD was in flight
					if (deleted) return; // the target is gone; don't un-mark the chip
					if (currentUuid !== forUuid) return; // superseded
					currentMime = meta.mime;
					refreshIcon();
					// The shared formatter renders "0 B" and doesn't guard
					// non-finite input; a chip with no known size should show
					// nothing at all, so the conditional lives here rather
					// than in the helper (PLAN-2392 DR-3b).
					const size =
						Number.isFinite(meta.size) && meta.size > 0 ? formatBytes(meta.size) : '';
					sizeEl.textContent = size ? `· ${size}` : '';
				});
			};

			probeMetadata(currentUuid);

			return {
				dom: wrapper,
				// Refresh the live chip in place when attrs change. Without
				// this, ProseMirror destroys + recreates the NodeView on any
				// attr update — fine today (no in-tab attr-change source for
				// chips), but the upcoming Yjs collab work makes peer-driven
				// uuid/filename changes the common case. Same shape as
				// AttachmentImage's update() hook (TASK-1250).
				update(updatedNode: ProseMirrorNode) {
					if (updatedNode.type.name !== node.type.name) return false;

					const newUuid = (updatedNode.attrs.uuid as string | null) ?? '';
					const newFilename = (updatedNode.attrs.filename as string | null) ?? '';

					if (newUuid !== currentUuid) {
						currentUuid = newUuid;
						// New target ⇒ the old deletion no longer applies.
						deleted = false;
						wrapper.classList.remove('attachment-missing');
						wrapper.removeAttribute('title');
						// New uuid ⇒ stale MIME / size; reset until HEAD probe
						// returns for the new identifier.
						currentMime = null;
						sizeEl.textContent = '';
						refreshHref();
						refreshIcon();
						probeMetadata(newUuid);
					}
					if (newFilename !== currentFilename) {
						currentFilename = newFilename;
						refreshFilenameDom();
						// Filename feeds the fallback icon whenever the MIME
						// path doesn't produce a specific icon — both MIME
						// unknown AND MIME known-but-unmapped (e.g. generic
						// application/octet-stream). Always recomputing is
						// idempotent for MIMEs with a definitive icon.
						refreshIcon();
					}

					return true;
				},
				destroy() {
					destroyed = true;
					disposeDeletionListener();
				},
			};
		};
	},

	addStorage() {
		return {
			markdown: {
				serialize(
					state: { write: (s: string) => void },
					node: { attrs: { uuid: unknown; filename: unknown } },
				) {
					const uuid = node.attrs.uuid;
					if (typeof uuid !== 'string' || uuid === '') return;
					const filename = typeof node.attrs.filename === 'string' ? node.attrs.filename : '';
					// Escape `]` and `\` in the filename so the markdown link
					// label stays balanced. The Go-side resolver and TS marked
					// renderer both unescape these, so the round-trip is
					// idempotent. Forward slashes in filenames are fine.
					const escaped = filename.replace(/\\/g, '\\\\').replace(/]/g, '\\]');
					state.write(`[${escaped}](${PAD_ATTACHMENT_PREFIX}${uuid})`);
				},
				parse: {
					// markdown-it's link token already produces
					// <a href="pad-attachment:UUID">filename</a>; our parseHTML
					// rules pick that up. No custom markdown-it rule needed.
				},
			},
		};
	},

	addCommands() {
		return {
			setAttachmentChip:
				(options) =>
				({ commands }) =>
					commands.insertContent({
						type: this.name,
						attrs: { uuid: options.uuid, filename: options.filename },
					}),
		};
	},
});
