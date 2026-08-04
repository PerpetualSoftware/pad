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
 * Activation (TASK-2424 / PLAN-2392 DR-2): clicking — or pressing Enter or
 * Space on — a live chip opens the shared attachment OPTIONS PANEL, the same
 * one the item attachment strip's file tiles open, rather than the old
 * `window.open` of the download URL. The NodeView can't mount a Svelte
 * component, so it signals the owning `ItemDetail` host through the events
 * bus, stamped with the host address its options carry. `renderHTML` below is
 * unchanged: it is the non-NodeView / clipboard shape, where an `<a download>`
 * is still the honest representation.
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
import {
	notifyAttachmentPanelOpen,
	registerAttachmentDeletionListener
} from '$lib/attachments/events';
import {
	type AttachmentHostAddressReader,
	readUnaddressed
} from '$lib/attachments/hostAddress';
import { describeAttachmentType, formatBytes, iconForAttachment } from '$lib/attachments/display';
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
	/**
	 * Reads the host address (item + owning `ItemDetail` mount) to stamp on
	 * open-panel events (PLAN-2392 DR-8). A reader rather than two strings
	 * because one host is reused across an item switch and Tiptap options
	 * cannot be written after configure — see `$lib/attachments/hostAddress`.
	 */
	address: AttachmentHostAddressReader;
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
			address: readUnaddressed,
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
			// Kept alongside the rendered text because the open-panel event
			// carries the three metadata fields the panel displays (DR-2), and
			// the accessible name below names the size too. Null until the HEAD
			// probe resolves — legitimately so: the panel completes what the
			// chip doesn't know rather than waiting for it.
			let currentSize: number | null = null;

			// A BUTTON, not an anchor (DR-12; orchestrator review of TASK-2424).
			// A live chip opens the options panel — it does not navigate — so an
			// anchor was announcing it as a link and, worse, left the URL
			// reachable by paths the click handler never sees: middle-click and
			// aux-click would still open or download it, straight past the
			// affordance that exists to stop a tap doing that. Button semantics
			// remove the bypass by construction rather than intercepting it, and
			// match the strip's file tile, which is the same control.
			//
			// `renderHTML` stays an `<a download>`: that is the clipboard /
			// non-NodeView shape, where a real link IS the honest representation
			// and there is no panel to open.
			const wrapper = document.createElement('button');
			wrapper.type = 'button';
			wrapper.className = 'file-chip';
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

			// The id is the chip's identity for the deletion bus and for the
			// panel event; the download URL is the PANEL's business now, so the
			// element carries no href to be aux-clicked.
			const refreshHref = (): void => {
				if (currentUuid) {
					wrapper.setAttribute('data-attachment-id', currentUuid);
				} else {
					wrapper.removeAttribute('data-attachment-id');
				}
			};

			// `data-filename` only: a `download` attribute means nothing on a
			// button, and the actual download is the panel's Download action,
			// which sets it on a real anchor (DR-16).
			const refreshFilenameDom = (): void => {
				if (currentFilename) {
					wrapper.setAttribute('data-filename', currentFilename);
				} else {
					wrapper.removeAttribute('data-filename');
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
			 * Accessible name: filename, type AND the action (PLAN-2392 DR-12).
			 *
			 * Activating a chip opens the options panel rather than the file, and
			 * nothing else on the chip says so — the icon is aria-hidden and the
			 * visible text is just the name. Recomputed whenever any of the three
			 * inputs changes, since the size and the MIME arrive asynchronously.
			 *
			 * Size is included only once known: an unresolved probe should read
			 * as "Options for notes.txt, Text", not as a confident "0 B".
			 *
			 * Deleted-state aware, and deliberately so rather than the dead
			 * wording living only in `markDeleted`: a filename update on an
			 * already-dead chip calls back through here, and a name that went
			 * back to promising options for a row that is gone would be worse
			 * than no name at all. (`deleted` is declared below and only ever
			 * read after it is initialised.)
			 */
			const refreshAccessibleName = (): void => {
				const name = currentFilename || 'attachment';
				if (deleted) {
					wrapper.setAttribute('aria-label', `${name} — this attachment has been deleted`);
					return;
				}
				const parts = [name, describeAttachmentType(currentMime, currentFilename)];
				if (Number.isFinite(currentSize) && (currentSize as number) > 0) {
					parts.push(formatBytes(currentSize as number));
				}
				wrapper.setAttribute('aria-label', `Options for ${parts.join(', ')}`);
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
				// `disabled` is what makes the dead chip genuinely INERT (DR-12),
				// not merely unclickable: a disabled button is unfocusable and
				// receives no click or keydown at all, so a keyboard user is
				// never handed a focus stop whose Enter and Space do nothing.
				// `tabindex` is deliberately never set for the same reason.
				//
				// Blur it explicitly first: a chip the user is focused on RIGHT
				// NOW (they tabbed to it, then another surface deleted the row)
				// stays focused in some browsers even once disabled, which would
				// strand focus on an element that no longer responds.
				if (typeof document !== 'undefined' && document.activeElement === wrapper) {
					wrapper.blur();
				}
				wrapper.disabled = true;
				wrapper.title = 'This attachment has been deleted';
				currentSize = null;
				sizeEl.textContent = '';
				refreshAccessibleName();
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
			refreshAccessibleName();

			/**
			 * Open the options panel for this chip (PLAN-2392 DR-2 / TASK-2424).
			 *
			 * Replaces the old `window.open` of the download URL: a chip and a
			 * strip tile are the same attachment and now behave identically —
			 * metadata first, Download as a deliberate choice.
			 *
			 * The address is read AT EMIT TIME (`options.address()`), never
			 * cached: the comment composer is reused across an item switch, so a
			 * value captured at configure() would address the previous item's
			 * host. Tiptap's `options` is a getter returning a fresh spread, so
			 * pushing a new value onto it after configure() is a silent no-op —
			 * hence a reader. See `$lib/attachments/hostAddress`.
			 *
			 * An UNADDRESSED editor (no host token — no `ItemDetail` above it)
			 * emits nothing: `notifyAttachmentPanelOpen` drops it rather than
			 * broadcasting to every mounted host, which is DR-8's whole point.
			 * Every live mount site threads the address; a surface that doesn't
			 * has no panel to open.
			 *
			 * Reads `currentUuid` (mutable) so a peer Yjs op swapping the chip's
			 * target is honoured at activation time.
			 */
			const openPanel = (): void => {
				if (!currentUuid || deleted) return;
				const address = this.options.address();
				notifyAttachmentPanelOpen({
					attachmentId: currentUuid,
					itemId: address.itemId,
					hostToken: address.hostToken,
					anchor: wrapper,
					filename: currentFilename || null,
					mime_type: currentMime,
					size_bytes: currentSize,
				});
			};

			// Explicit click handler. Editor.svelte installs a global
			// anchor-click suppressor that calls preventDefault on every <a>
			// inside the editor (so plain text links don't navigate in edit
			// mode); without this handler the chip's activation would also be
			// eaten and clicking the chip would silently do nothing. Mirrors the
			// pattern AttachmentImage uses for its lightbox click.
			//
			// This is the MOUSE activation path. Keyboard activation is the
			// keydown handler below and never reaches here — see why there.
			wrapper.addEventListener('click', (event) => {
				if (event.detail > 1) return; // double-click → fall through
				if (!currentUuid) return;
				// Removing href is not enough: this handler acts on the URL
				// itself, so a deleted chip would still do something (a 404 in a
				// new tab, before TASK-2424; a panel for a dead row, after).
				// Swallow the click instead (Codex round 14).
				event.preventDefault();
				event.stopPropagation();
				if (deleted) return;
				openPanel();
			});

			/**
			 * Keyboard activation: Enter AND Space, exactly once each (DR-12).
			 *
			 * Enter is handled HERE rather than left to the anchor's native
			 * "Enter means click", for a reason specific to living inside a
			 * ProseMirror editor: the chip sits in the editable region, so an
			 * un-suppressed Enter bubbles to the editor's own keymap, which
			 * treats it as split-block — it calls `preventDefault` itself, which
			 * ALSO cancels the anchor's activation click. Relying on the native
			 * path would mean Enter silently split a paragraph instead of
			 * opening the panel.
			 *
			 * `preventDefault` here is what keeps the count at one: a cancelled
			 * keydown produces no activation click, so this handler and the
			 * click handler above are disjoint rather than racing (the exact
			 * double-fire DR-12 names). It also suppresses Space's page scroll,
			 * and `stopPropagation` keeps both keys away from the editor keymap.
			 */
			wrapper.addEventListener('keydown', (event) => {
				const isSpace = event.key === ' ' || event.key === 'Spacebar';
				if (event.key !== 'Enter' && !isSpace) return;
				// A modified key is a shortcut, not an activation.
				if (event.ctrlKey || event.metaKey || event.altKey || event.shiftKey) return;
				// Suppress BEFORE the deleted/no-uuid bail, not after. A disabled
				// button gets no keydown, so this is belt-and-braces — but if a
				// dead chip ever did receive one, returning early would let Enter
				// through to ProseMirror's keymap and split the paragraph the
				// chip sits in, which is a destructive answer to pressing Enter
				// on something inert.
				event.preventDefault();
				event.stopPropagation();
				if (!currentUuid || deleted) return;
				openPanel();
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
				).then((result) => {
					if (destroyed) return; // NodeView torn down while HEAD was in flight
					if (deleted) return; // the target is gone; don't un-mark the chip
					if (currentUuid !== forUuid) return; // superseded
					// A transient failure says nothing about whether the row
					// exists — keep the filename-guess icon and stay
					// retryable (PLAN-2392 DR-17).
					if (result.status === 'transient') return;
					// A 404 IS authoritative. This is the path editor undo
					// takes: undo restores the chip node, but the delete was a
					// REST row mutation Tiptap's history can't roll back, so
					// the chip must render dead rather than link to a 404.
					if (result.status === 'missing') {
						markDeleted();
						return;
					}
					currentMime = result.mime;
					currentSize = result.size;
					refreshIcon();
					// The shared formatter renders "0 B" and doesn't guard
					// non-finite input; a chip with no known size should show
					// nothing at all, so the conditional lives here rather
					// than in the helper (PLAN-2392 DR-3b).
					const size =
						Number.isFinite(result.size) && result.size > 0 ? formatBytes(result.size) : '';
					sizeEl.textContent = size ? `· ${size}` : '';
					// The name says the type and the size; both just arrived.
					refreshAccessibleName();
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
						// New target ⇒ the old deletion no longer applies. Undoing
						// EVERY part of markDeleted() matters: `disabled` is what
						// makes a dead chip inert, so leaving it set here would
						// give a live chip that announces itself as live and does
						// nothing — worse than the dead one, which at least says
						// so. Reachable via a peer's uuid swap or a ProseMirror
						// node replacement (orchestrator's full-diff review).
						deleted = false;
						wrapper.disabled = false;
						wrapper.classList.remove('attachment-missing');
						wrapper.removeAttribute('title');
						// New uuid ⇒ stale MIME / size; reset until HEAD probe
						// returns for the new identifier.
						currentMime = null;
						currentSize = null;
						sizeEl.textContent = '';
						refreshHref();
						refreshIcon();
						refreshAccessibleName();
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
						refreshAccessibleName();
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
