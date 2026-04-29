/**
 * AttachmentUpload — Tiptap extension that intercepts paste and drop
 * events with files and uploads them through the attachment API.
 *
 * Flow:
 *   1. User pastes a screenshot or drops a file onto the editor.
 *   2. The plugin detects file payloads in clipboardData / dataTransfer.
 *   3. For each file: insert a position-tracked placeholder (decoration
 *      widget — zero document width so it never enters serialized
 *      markdown), kick off `api.attachments.upload`, race the network
 *      with continued editing.
 *   4. On success: replace the placeholder with an `attachmentImage`
 *      node (image MIME) or `attachmentChip` node (everything else),
 *      using the position the plugin's transaction mapping has carried
 *      forward across intervening edits.
 *   5. On error: remove the placeholder and surface the failure via the
 *      injected `onError` callback. The placeholder is visual-only so
 *      removing it leaves the document otherwise untouched.
 *
 * The uploaded blob is dropped silently if its placeholder has been
 * deleted before completion (user changed their mind, navigated away,
 * etc.). Orphan-GC reclaims the on-disk bytes after the grace period.
 *
 * Multiple files in a single drop fan out as concurrent uploads with
 * individual placeholders — the user sees one spinner per file and
 * each replaces its own placeholder when its upload finishes, in
 * whatever order the network completes.
 */

import { Extension } from '@tiptap/core';
import { Plugin, PluginKey } from '@tiptap/pm/state';
import { Decoration, DecorationSet, type EditorView } from '@tiptap/pm/view';
import type { AttachmentUploadResult } from '$lib/types';

/** Per-upload metadata stored in plugin state and rendered as a decoration. */
interface UploadEntry {
	pos: number;
	filename: string;
}

interface UploadState {
	uploads: Map<string, UploadEntry>;
}

interface UploadAction {
	/** Add a new upload placeholder. */
	add?: { id: string; pos: number; filename: string };
	/** Remove a placeholder by id (success or cancellation). */
	remove?: string;
}

export interface AttachmentUploadOptions {
	/** Upload a single file; returns the persisted attachment metadata. */
	upload: (file: File) => Promise<AttachmentUploadResult>;
	/**
	 * Called when an upload fails or the file's MIME is rejected. The
	 * plugin doesn't render its own error UI — the editor wires this to
	 * the host app's notification system.
	 */
	onError?: (filename: string, message: string) => void;
}

const pluginKey = new PluginKey<UploadState>('attachmentUpload');

/**
 * Crypto-strong-ish placeholder id. crypto.randomUUID is the preferred
 * source; the Math.random fallback is only hit in environments without
 * the WebCrypto API (server-side rendering, ancient browsers) where
 * the upload plugin can't run anyway. Collision-resistant within the
 * scope of a single editing session, which is all we need.
 */
function newId(): string {
	if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
		return crypto.randomUUID();
	}
	return `up-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
}

/** Build the placeholder DOM. Kept tiny — purely visual, ignored by ProseMirror's selection model. */
function buildPlaceholder(filename: string): HTMLElement {
	const wrapper = document.createElement('span');
	wrapper.className = 'attachment-upload-placeholder';
	wrapper.contentEditable = 'false';

	const spinner = document.createElement('span');
	spinner.className = 'attachment-upload-spinner';
	spinner.setAttribute('aria-hidden', 'true');

	const label = document.createElement('span');
	label.className = 'attachment-upload-label';
	label.textContent = `Uploading ${filename}…`;

	wrapper.append(spinner, label);
	return wrapper;
}

/**
 * Extract file payloads from a paste event. Falls back to the empty
 * array when the clipboard has no files (regular text paste, etc.) so
 * the caller can cleanly defer to the default paste handler.
 */
function filesFromPaste(event: ClipboardEvent): File[] {
	if (!event.clipboardData) return [];
	const out: File[] = [];
	for (const item of Array.from(event.clipboardData.items)) {
		if (item.kind !== 'file') continue;
		const file = item.getAsFile();
		if (file) out.push(file);
	}
	return out;
}

/** Same idea for drop events — covers the dataTransfer.files surface. */
function filesFromDrop(event: DragEvent): File[] {
	const dt = event.dataTransfer;
	if (!dt || dt.files.length === 0) return [];
	return Array.from(dt.files);
}

/**
 * Resolve the document position to insert at for a drop event. Uses
 * the editor's coordinate-to-position mapping; returns null when the
 * drop landed outside the editor body, so the handler can fall through.
 */
function dropPosition(view: EditorView, event: DragEvent): number | null {
	const coords = view.posAtCoords({ left: event.clientX, top: event.clientY });
	return coords?.pos ?? null;
}

/**
 * Kick off an upload for a single file. Inserts the placeholder
 * immediately, then races the network. The placeholder's position is
 * mapped through every intervening transaction by the plugin's
 * `apply`, so the final replace lands at the right spot even if the
 * user keeps typing.
 */
function startUpload(
	view: EditorView,
	file: File,
	insertPos: number,
	opts: AttachmentUploadOptions,
): void {
	const id = newId();
	const filename = file.name || 'attachment';

	// 1. Add placeholder. The transaction is dispatched synchronously so
	//    the user sees the spinner the moment the paste/drop lands.
	view.dispatch(
		view.state.tr.setMeta(pluginKey, {
			add: { id, pos: insertPos, filename },
		} satisfies UploadAction),
	);

	// 2. Race the network. We re-read state on resolve because the user
	//    may have edited around the placeholder (the position has been
	//    mapped) or removed it entirely (the placeholder is gone).
	opts
		.upload(file)
		.then((result) => {
			const state = pluginKey.getState(view.state);
			const entry = state?.uploads.get(id);
			if (!entry) return; // placeholder gone — drop the upload silently
			const tr = view.state.tr;
			const node = nodeForResult(view, result);
			if (node) tr.insert(entry.pos, node);
			tr.setMeta(pluginKey, { remove: id } satisfies UploadAction);
			view.dispatch(tr);
		})
		.catch((err: unknown) => {
			view.dispatch(view.state.tr.setMeta(pluginKey, { remove: id } satisfies UploadAction));
			const message = err instanceof Error ? err.message : String(err ?? 'Upload failed');
			opts.onError?.(filename, message);
		});
}

/**
 * Build the node that should replace a placeholder for a given upload
 * result. Image MIMEs become `attachmentImage`; everything else becomes
 * `attachmentChip`. Returns null when the schema has neither node
 * registered (e.g. the editor was set up without those extensions) —
 * the placeholder is removed and the upload silently lands as an
 * orphan, which orphan-GC will reclaim later.
 */
function nodeForResult(view: EditorView, result: AttachmentUploadResult) {
	const schema = view.state.schema;
	const isImage = result.category === 'image';
	if (isImage && schema.nodes.attachmentImage) {
		return schema.nodes.attachmentImage.create({ uuid: result.id, alt: result.filename });
	}
	if (schema.nodes.attachmentChip) {
		return schema.nodes.attachmentChip.create({ uuid: result.id, filename: result.filename });
	}
	return null;
}

/** Build the ProseMirror plugin. Exposed for tests / advanced wiring. */
export function attachmentUploadPlugin(opts: AttachmentUploadOptions): Plugin<UploadState> {
	return new Plugin<UploadState>({
		key: pluginKey,
		state: {
			init: () => ({ uploads: new Map() }),
			apply(tr, prev): UploadState {
				// Map every placeholder's position through the new
				// transaction so they track the user's edits. We always
				// produce a fresh Map so the previous state object is
				// never mutated — ProseMirror compares state objects by
				// reference for change detection.
				const next: UploadState = { uploads: new Map() };
				for (const [id, entry] of prev.uploads) {
					next.uploads.set(id, { ...entry, pos: tr.mapping.map(entry.pos) });
				}
				const action = tr.getMeta(pluginKey) as UploadAction | undefined;
				if (action?.add) {
					next.uploads.set(action.add.id, {
						pos: action.add.pos,
						filename: action.add.filename,
					});
				}
				if (action?.remove) {
					next.uploads.delete(action.remove);
				}
				return next;
			},
		},
		props: {
			decorations(state) {
				const us = pluginKey.getState(state);
				if (!us || us.uploads.size === 0) return null;
				const decos: Decoration[] = [];
				for (const [, entry] of us.uploads) {
					decos.push(
						Decoration.widget(entry.pos, () => buildPlaceholder(entry.filename), {
							// side: -1 so the widget renders BEFORE the cursor
							// position. This keeps the placeholder anchored
							// to where the file was dropped/pasted, rather
							// than drifting to the right as the user types.
							side: -1,
							// ignoreSelection prevents ProseMirror from
							// putting the cursor inside the placeholder DOM
							// when the user clicks on it.
							ignoreSelection: true,
						}),
					);
				}
				return DecorationSet.create(state.doc, decos);
			},
			handleDOMEvents: {
				paste(view, event) {
					const files = filesFromPaste(event as ClipboardEvent);
					if (files.length === 0) return false; // let other handlers process the paste
					event.preventDefault();
					const pos = view.state.selection.from;
					for (const file of files) {
						startUpload(view, file, pos, opts);
					}
					return true;
				},
				drop(view, event) {
					const files = filesFromDrop(event as DragEvent);
					if (files.length === 0) return false;
					const pos = dropPosition(view, event as DragEvent);
					if (pos === null) return false;
					event.preventDefault();
					for (const file of files) {
						startUpload(view, file, pos, opts);
					}
					return true;
				},
			},
		},
	});
}

/**
 * Tiptap-flavoured wrapper. Lets Editor.svelte add the upload behavior
 * via the standard `extensions` array alongside StarterKit, etc.
 */
export const AttachmentUpload = Extension.create<AttachmentUploadOptions>({
	name: 'attachmentUpload',

	addOptions() {
		return {
			upload: async () => {
				throw new Error('AttachmentUpload: configure({ upload }) is required');
			},
			onError: undefined,
		};
	},

	addProseMirrorPlugins() {
		return [attachmentUploadPlugin(this.options)];
	},
});
