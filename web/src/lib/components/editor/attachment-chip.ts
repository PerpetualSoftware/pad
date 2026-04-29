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
 * canonical category icon, and the size is rendered alongside the name.
 */

import { Node, mergeAttributes } from '@tiptap/core';
import {
	type AttachmentUrlBuilder,
	type AttachmentVariant,
	fetchAttachmentMetadata
} from './attachment-metadata';

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

/**
 * Map a MIME type to a category icon. Mirrors the server-side category
 * enum in `internal/attachments/mime.go`. When MIME is unknown (HEAD
 * failed or hasn't returned yet), falls back to a filename-extension
 * heuristic — same buckets, same icons, just a coarser source.
 */
function iconForMime(mime: string): string {
	const m = mime.toLowerCase();
	if (m.startsWith('image/')) return '🖼️';
	if (m.startsWith('video/')) return '🎥';
	if (m.startsWith('audio/')) return '🎵';
	if (m === 'application/pdf') return '📑';
	if (
		m === 'application/zip' ||
		m === 'application/x-tar' ||
		m === 'application/gzip' ||
		m === 'application/x-bzip2' ||
		m === 'application/x-7z-compressed'
	)
		return '🗜️';
	if (
		m === 'application/msword' ||
		m === 'application/rtf' ||
		m.includes('wordprocessingml') ||
		m.includes('opendocument.text')
	)
		return '📄';
	if (
		m === 'application/vnd.ms-excel' ||
		m.includes('spreadsheetml') ||
		m.includes('opendocument.spreadsheet') ||
		m === 'text/csv' ||
		m === 'text/tab-separated-values'
	)
		return '📊';
	if (
		m === 'application/vnd.ms-powerpoint' ||
		m.includes('presentationml') ||
		m.includes('opendocument.presentation')
	)
		return '📽️';
	if (
		m === 'text/plain' ||
		m === 'text/markdown' ||
		m === 'application/json' ||
		m === 'application/xml' ||
		m === 'text/xml' ||
		m === 'application/yaml' ||
		m === 'text/yaml' ||
		m === 'application/toml'
	)
		return '📝';
	return '';
}

function iconForFilename(filename: string): string {
	const ext = filename.toLowerCase().match(/\.([a-z0-9]+)$/)?.[1] ?? '';
	if (['pdf'].includes(ext)) return '📑';
	if (['zip', 'tar', 'gz', '7z', 'bz2'].includes(ext)) return '🗜️';
	if (['doc', 'docx', 'odt', 'rtf'].includes(ext)) return '📄';
	if (['xls', 'xlsx', 'ods', 'csv', 'tsv'].includes(ext)) return '📊';
	if (['ppt', 'pptx', 'odp'].includes(ext)) return '📽️';
	if (['mp4', 'webm', 'mov', 'mkv', 'avi'].includes(ext)) return '🎥';
	if (['mp3', 'wav', 'ogg', 'flac', 'aac', 'm4a'].includes(ext)) return '🎵';
	if (['png', 'jpg', 'jpeg', 'gif', 'webp', 'avif', 'heic', 'heif'].includes(ext)) return '🖼️';
	if (['txt', 'md', 'json', 'yaml', 'yml', 'xml', 'toml', 'html', 'js', 'ts'].includes(ext)) return '📝';
	return '📎';
}

/** Format a byte count as a human-readable string ("832 B", "1.2 MB", "5 GB"). */
function formatBytes(n: number): string {
	if (!Number.isFinite(n) || n <= 0) return '';
	if (n < 1024) return `${n} B`;
	const units = ['KB', 'MB', 'GB', 'TB'];
	let val = n / 1024;
	let i = 0;
	while (val >= 1024 && i < units.length - 1) {
		val /= 1024;
		i++;
	}
	return `${val < 10 ? val.toFixed(1) : Math.round(val).toString()} ${units[i]}`;
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
			const uuid = (node.attrs.uuid as string | null) ?? '';
			const filename = (node.attrs.filename as string | null) ?? '';

			const wrapper = document.createElement('a');
			wrapper.className = 'file-chip';
			wrapper.target = '_blank';
			wrapper.rel = 'noopener noreferrer';
			wrapper.contentEditable = 'false';
			if (uuid) {
				wrapper.href = this.options.getDownloadUrl(uuid);
				wrapper.setAttribute('data-attachment-id', uuid);
			}
			if (filename) wrapper.setAttribute('data-filename', filename);
			if (filename) wrapper.download = filename;

			// Explicit click handler → window.open. Editor.svelte installs a
			// global anchor-click suppressor that calls preventDefault on
			// every <a> inside the editor (so plain text links don't navigate
			// in edit mode); without this handler the chip's anchor
			// navigation would also be eaten and clicking the chip would
			// silently do nothing. Mirrors the pattern AttachmentImage uses
			// for its lightbox click. stopImmediatePropagation prevents the
			// global suppressor from running afterwards.
			wrapper.addEventListener('click', (event) => {
				if (event.detail > 1) return; // double-click → fall through
				if (!uuid) return;
				event.preventDefault();
				event.stopPropagation();
				if (typeof window !== 'undefined') {
					window.open(
						this.options.getDownloadUrl(uuid),
						'_blank',
						'noopener,noreferrer',
					);
				}
			});

			const iconEl = document.createElement('span');
			iconEl.className = 'file-chip-icon';
			iconEl.setAttribute('aria-hidden', 'true');
			iconEl.textContent = iconForFilename(filename);

			const nameEl = document.createElement('span');
			nameEl.className = 'file-chip-name';
			nameEl.textContent = filename || 'attachment';

			const sizeEl = document.createElement('span');
			sizeEl.className = 'file-chip-size';
			// Empty until metadata resolves; CSS hides empty separator span.

			wrapper.append(iconEl, nameEl, sizeEl);

			// Async metadata enrichment via HEAD. Server registers HEAD
			// alongside GET (chi doesn't auto-route HEAD to GET handlers),
			// and the response carries Content-Type + Content-Length without
			// a body. The promise cache keyed by (workspace, uuid)
			// deduplicates repeated chips for the same attachment and
			// survives undo/redo. Skip the fetch when no workspace context
			// is available (e.g. headless rendering) — the chip still works,
			// just without size/icon refinement.
			if (uuid && this.options.workspaceSlug) {
				fetchAttachmentMetadata(
					this.options.workspaceSlug,
					uuid,
					this.options.getDownloadUrl,
				).then((meta) => {
					if (!meta) return;
					const refined = iconForMime(meta.mime);
					if (refined) iconEl.textContent = refined;
					const size = formatBytes(meta.size);
					if (size) sizeEl.textContent = `· ${size}`;
				});
			}

			return { dom: wrapper };
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
