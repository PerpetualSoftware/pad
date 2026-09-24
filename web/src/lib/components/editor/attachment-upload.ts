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
 * A FROZEN editor (read-only while a detail pane is open over it, the
 * master freeze of TASK-2172 / TASK-2180) defers rather than drops
 * (BUG-2177). Every entry point refuses to START an upload on a read-only
 * view, so an upload in flight at freeze time was begun by the user while
 * the editor was editable. Its result is parked on the placeholder entry,
 * which keeps being mapped through every transaction (peers' included), and
 * inserted by the plugin view's `update` once the view is editable again.
 * Where it can no longer be inserted — the view was torn down — the host is
 * told through `onNotice`, naming the file, because the bytes are stored
 * server-side and nothing references them.
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

declare module '@tiptap/core' {
	interface Commands<ReturnType> {
		attachmentUpload: {
			/**
			 * Upload one or more files through the same pipeline as
			 * paste/drop, inserting each at the current selection. Backs
			 * the toolbar / slash "Attach file" buttons — the surface a
			 * touch device (which can't paste/drop a file) relies on.
			 */
			uploadAttachments: (files: File[]) => ReturnType;
		};
	}
}

/** Per-upload metadata stored in plugin state and rendered as a decoration. */
interface UploadEntry {
	pos: number;
	filename: string;
	/** Set when the upload finished while the view was read-only (BUG-2177):
	 *  the result waits here, on the mapped placeholder, for the thaw. */
	result?: AttachmentUploadResult;
	/** The placeholder's position was deleted outright (the content around it
	 *  removed, or the whole document replaced), so a mapped number survives
	 *  but the place the user dropped the file does not. Inserting there would
	 *  put the file at a deletion boundary, possibly in content the user never
	 *  saw (codex round 1 on BUG-2177), so a lost placeholder is reported. */
	lost?: boolean;
}

interface UploadState {
	uploads: Map<string, UploadEntry>;
}

interface UploadAction {
	/** Add a new upload placeholder. */
	add?: { id: string; pos: number; filename: string };
	/** Remove a placeholder by id (success or cancellation). */
	remove?: string;
	/** Park a finished upload's result on its placeholder (frozen view, BUG-2177). */
	ready?: { id: string; result: AttachmentUploadResult };
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
	/**
	 * Called when an upload SUCCEEDED but its result could not be inserted
	 * (BUG-2177): the file is stored server-side and nothing references it.
	 * The message names the file. Wired to the host's notification system.
	 */
	onNotice?: (message: string) => void;
}

/** The notice for a stored upload whose reference never landed (BUG-2177). */
export function storedNotInsertedMessage(filename: string): string {
	return `${filename} was uploaded but not inserted into the document: the editor closed, or the place it was dropped was removed, before it could be inserted. Attach it again to use it.`;
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
			// Master-freeze / R12 (TASK-2172): the view can be torn down or turned
			// read-only WHILE this upload is in flight — a genuine remount (item
			// switch / forceRefreshNonce) destroys THIS view, or a peeking master
			// flips it read-only in place (PLAN-2179 DR-1 / TASK-2180: `peeking` is
			// no longer in the `{#key}`, so a freeze flips `editable=false` on the
			// SAME view rather than remounting it). Never dispatch into a
			// destroyed/read-only view: dispatch on a destroyed view throws, and
			// inserting into a frozen one is exactly the mutation the freeze forbids.
			// Drop the result (the bytes are already stored server-side; the
			// reference is simply not inserted) and clean up the placeholder if the
			// same view still lives.
			// BUG-2177 changes the frozen arm from DROP to DEFER: the file was
			// uploaded at the user's request, so its reference is inserted when
			// the view thaws (see the plugin view's `update`), not discarded.
			// A DESTROYED view cannot take it at all, and the stored bytes would
			// be orphaned silently, so the host is told, naming the file.
			if (view.isDestroyed) {
				opts.onNotice?.(storedNotInsertedMessage(filename));
				return;
			}
			const state = pluginKey.getState(view.state);
			const entry = state?.uploads.get(id);
			if (!entry) return; // placeholder gone — drop the upload silently
			if (!view.editable) {
				// A meta-only transaction: no document step, so no Yjs op, which
				// is why it is allowed on a frozen view (the removal below it on
				// the error arm relies on the same property).
				view.dispatch(view.state.tr.setMeta(pluginKey, { ready: { id, result } } satisfies UploadAction));
				return;
			}
			const tr = view.state.tr;
			const node = entry.lost ? null : nodeForResult(view, result);
			if (node) tr.insert(entry.pos, node);
			tr.setMeta(pluginKey, { remove: id } satisfies UploadAction);
			view.dispatch(tr);
			if (!node) opts.onNotice?.(storedNotInsertedMessage(filename));
		})
		.catch((err: unknown) => {
			// A destroyed view can't accept the placeholder-cleanup dispatch. The
			// cleanup is a meta-only transaction (no doc step, so no Yjs op) and
			// is safe on a live view, frozen or not. The failure IS reported while
			// frozen (BUG-2177): every entry point refuses to start an upload on a
			// read-only view, so this one was begun by the user while the editor
			// was editable, and its failure is theirs to hear about. (The earlier
			// reasoning, "an upload it never let the user start", described a
			// paste onto a frozen view, which the handlers already refuse.)
			if (view.isDestroyed) return;
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

function hasReadyUploads(view: EditorView): boolean {
	const st = pluginKey.getState(view.state);
	if (!st) return false;
	for (const entry of st.uploads.values()) if (entry.result) return true;
	return false;
}

/** Insert every parked upload at its mapped placeholder (BUG-2177). One
 *  transaction per upload, each reading the state the previous one left, so
 *  no position is used after a sibling insertion shifted it. */
function insertReadyUploads(view: EditorView, opts: AttachmentUploadOptions): void {
	for (;;) {
		if (view.isDestroyed || !view.editable) return;
		const st = pluginKey.getState(view.state);
		if (!st) return;
		let ready: [string, UploadEntry] | undefined;
		for (const pair of st.uploads) {
			if (pair[1].result) {
				ready = pair;
				break;
			}
		}
		if (!ready) return;
		const [id, entry] = ready;
		const tr = view.state.tr;
		const node = entry.lost ? null : nodeForResult(view, entry.result!);
		if (node) tr.insert(entry.pos, node);
		tr.setMeta(pluginKey, { remove: id } satisfies UploadAction);
		view.dispatch(tr);
		if (!node) opts.onNotice?.(storedNotInsertedMessage(entry.filename));
	}
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
					const mapped = tr.mapping.mapResult(entry.pos, -1);
					next.uploads.set(id, {
						...entry,
						pos: mapped.pos,
						lost: entry.lost || mapped.deletedAcross,
					});
				}
				const action = tr.getMeta(pluginKey) as UploadAction | undefined;
				if (action?.add) {
					next.uploads.set(action.add.id, {
						pos: action.add.pos,
						filename: action.add.filename,
					});
				}
				if (action?.ready) {
					const entry = next.uploads.get(action.ready.id);
					if (entry) next.uploads.set(action.ready.id, { ...entry, result: action.ready.result });
				}
				if (action?.remove) {
					next.uploads.delete(action.remove);
				}
				return next;
			},
		},
		view(editorView) {
			// BUG-2177: insert uploads that finished while the view was frozen,
			// once it is editable again. ProseMirror calls a plugin view's
			// `update` on every view update, including the setProps an
			// `editable` flip performs (the block drag handle relies on the same
			// thing). Deferred to a microtask because dispatching from inside
			// `update` would re-enter the view update in progress.
			let scheduled = false;
			const schedule = (view: EditorView) => {
				if (scheduled || !view.editable) return;
				if (!hasReadyUploads(view)) return;
				scheduled = true;
				queueMicrotask(() => {
					scheduled = false;
					insertReadyUploads(view, opts);
				});
			};
			// A plugin view is also created when the plugin set is rebuilt, and
			// entries parked before that must not wait for an unrelated update.
			schedule(editorView);
			return {
				update(view) {
					schedule(view);
				},
				// The view is going away with results still parked: the files are
				// stored and nothing will reference them, so say so, as the
				// destroyed arm of the upload itself does (codex round 1).
				destroy() {
					const st = pluginKey.getState(editorView.state);
					if (!st) return;
					for (const entry of st.uploads.values()) {
						if (entry.result) opts.onNotice?.(storedNotInsertedMessage(entry.filename));
					}
				},
			};
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
					// Master-freeze / R12 (PLAN-2154 / TASK-2172): ProseMirror runs
					// custom DOM handlers even when the view is NOT editable, so a
					// paste onto a read-only editor (a peeking full-page master OR a
					// genuine view-only viewer) would otherwise upload + dispatch a
					// Yjs insertion. Bail while read-only. NOTE: this also closes the
					// same latent hole for view-only viewers, whose attachment
					// mutations were already broken (no provider to sync; the content
					// PATCH 403s) — a bug fix, not a regression of any working flow.
					if (!view.editable) return false;
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
					// Same editable gate as paste (TASK-2172 / R12).
					if (!view.editable) return false;
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
			onNotice: undefined,
		};
	},

	addProseMirrorPlugins() {
		return [attachmentUploadPlugin(this.options)];
	},

	addCommands() {
		return {
			// Programmatic entry point for the toolbar / slash "Attach
			// file" buttons. Routes each picked file through the exact
			// same startUpload flow as paste/drop (placeholder → upload →
			// replace with attachmentImage/attachmentChip), so behavior,
			// error handling, and image-vs-chip routing stay identical
			// across every insertion surface. Inserts at the current
			// selection; startUpload's position mapping carries the
			// placeholder forward across intervening edits.
			uploadAttachments:
				(files: File[]) =>
				({ view }) => {
					// Master-freeze / R12 (TASK-2172): refuse programmatic uploads
					// on a read-only editor (the invoking toolbar/slash surfaces are
					// already hidden while frozen — defense-in-depth).
					if (!view.editable) return false;
					if (!files.length) return false;
					const pos = view.state.selection.from;
					for (const file of files) {
						startUpload(view, file, pos, this.options);
					}
					return true;
				},
		};
	},
});
