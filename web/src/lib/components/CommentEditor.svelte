<script lang="ts">
	/**
	 * Lean WYSIWYG comment editor (TASK-1664 / PLAN-1662). A small, purpose-
	 * built Tiptap instance — NOT the heavy block editor (Editor.svelte). It
	 * reuses the shared attachment pipeline (AttachmentUpload plugin +
	 * AttachmentImage/AttachmentChip nodes) and tiptap-markdown so pasted/
	 * dropped images show as inline thumbnails while composing, yet the value
	 * round-trips to plain markdown — comment.body stays markdown, so display,
	 * search, and the orphan-GC are untouched.
	 *
	 * Deliberately excludes tables, task lists, slash commands, the block drag
	 * handle, the import-from-URL modal, and collaboration — comments don't
	 * need a document editor.
	 *
	 * Used for the new-comment composer, the reply box, and (TASK-1665) inline
	 * edit mode.
	 */
	import { onMount, onDestroy } from 'svelte';
	import { Editor } from '@tiptap/core';
	import StarterKit from '@tiptap/starter-kit';
	import Link from '@tiptap/extension-link';
	import Placeholder from '@tiptap/extension-placeholder';
	import { Markdown } from 'tiptap-markdown';
	import { api } from '$lib/api/client';
	import { notifyAttachmentUploaded, toUploadedAttachment } from '$lib/attachments/events';
	import { unescapeDocLinks } from '$lib/utils/markdown';
	import { AttachmentImage } from './editor/attachment-image';
	import { AttachmentChip } from './editor/attachment-chip';
	import { AttachmentUpload } from './editor/attachment-upload';
	import type { AttachmentHostAddress } from '$lib/attachments/hostAddress';

	interface Props {
		/** Initial markdown body. Parsed as markdown on mount. */
		content?: string;
		placeholder?: string;
		/** Workspace slug — required for attachment upload + image URLs. */
		wsSlug: string;
		/**
		 * UUID of the item this comment belongs to. Sent with attachment
		 * uploads so the server can authorize against the item's grant chain
		 * — a grant-based editor (share-link guest, no workspace editor role)
		 * can attach instead of hitting a 403 (BUG-1661). Optional; without
		 * it uploads fall back to the workspace editor-role gate.
		 */
		itemId?: string;
		/**
		 * Identity of the `ItemDetail` mount that owns this composer
		 * (PLAN-2392 DR-8 / TASK-2421). Threaded down from ItemDetail through
		 * ItemTimeline (and TimelineCommentCard for edits/replies) so an
		 * attachment chip in a comment body can address the ONE host that
		 * owns it — a master and a peeked pane are both mounted, and `itemId`
		 * alone would let both consume the same event. Empty (the default,
		 * for composers mounted outside an ItemDetail) disables addressing
		 * rather than broadcasting.
		 */
		hostToken?: string;
		/** Label for the submit button (e.g. "Comment", "Reply", "Save"). */
		submitLabel?: string;
		/** External busy flag (network in flight in the host). */
		submitting?: boolean;
		autofocus?: boolean;
		/** Show a Cancel button + enable Esc-to-cancel (reply / edit mode). */
		onCancel?: () => void;
		/**
		 * Called with the current markdown when the user submits. May return a
		 * promise; on resolution the editor clears (composer behaviour). If it
		 * throws, the draft is kept so the user can retry.
		 */
		onSubmit: (markdown: string) => void | Promise<void>;
	}

	let {
		content = '',
		placeholder = 'Write a comment…',
		wsSlug,
		itemId,
		hostToken = '',
		submitLabel = 'Comment',
		submitting = false,
		autofocus = false,
		onCancel,
		onSubmit
	}: Props = $props();

	let element: HTMLDivElement | undefined = $state();
	let editor: Editor | undefined;
	let pendingUploads = $state(0);
	let empty = $state(true);
	let saving = $state(false);

	let busy = $derived(submitting || saving || pendingUploads > 0);

	function currentMarkdown(): string {
		if (!editor) return '';
		return unescapeDocLinks((editor.storage as any).markdown?.getMarkdown?.() ?? '').trim();
	}

	/**
	 * Appends markdown to the composer from OUTSIDE the component, without
	 * remounting it (IDEA-2843). The selection toolbar's "Comment" action uses
	 * this to drop a blockquote of the reader's selection into the live
	 * composer; the blockquote grammar is the CALLER's, so this stays a
	 * general append.
	 *
	 * Why an imperative handle and not the `content` prop: `content` is read
	 * ONCE, inside `new Editor({...})` in `onMount` below, and this component
	 * has no `$effect` syncing it afterwards. Writing the prop on a mounted
	 * composer is a SILENT no-op — no error, no warning, the text is simply
	 * dropped. A `{#key}` remount would work but would destroy an in-progress
	 * draft, which is the thing `doSubmit`'s identity capture (PLAN-2105 /
	 * TASK-2112) exists to protect.
	 *
	 * Append, never replace: a draft the user has already typed is kept and the
	 * addition goes after a blank line, so quoting a second passage into the
	 * same comment works without losing the first. Returns false when there was
	 * nothing to insert or no live editor, so a caller can tell the difference
	 * between "inserted" and "silently did nothing" — the failure mode this
	 * handle exists to remove.
	 */
	export function appendMarkdown(markdown: string): boolean {
		if (!editor || editor.isDestroyed) return false;
		const addition = markdown.trim();
		if (addition === '') return false;
		const existing = currentMarkdown();
		const next = existing === '' ? addition : `${existing}\n\n${addition}`;
		// setContent (block parse) rather than insertContentAt, and the reason
		// is a PREFERENCE, not a defect avoided: both routes preserve the
		// blockquote today. That was measured — swapping this line for
		// insertContentAt leaves the whole suite green. What separates them is
		// what they depend on. tiptap-markdown overrides insertContentAt to
		// parse with `{ inline: true }`, and the block structure survives that
		// only because normalizeInline unwraps the first child when it is a
		// <p> and a blockquote is not one. setContent parses in block mode and
		// depends on nothing of the sort.
		//
		// Nothing enforces this choice — a source guard for it would be a
		// scanner with an unbounded tail, which is not worth it here. If a
		// future edit moves to insertContentAt, the suite will stay green and
		// this comment is the only warning that the quote's block structure
		// then rides on a library internal.
		//
		// The round trip through markdown is not new loss: the composer already
		// parses markdown at mount and serializes it on every submit, so both
		// directions are the component's existing contract.
		// `empty` (which gates the submit button) is maintained by the editor's
		// own onUpdate: setContent's `emitUpdate` defaults true in
		// @tiptap/core 3.30.2. An explicit `empty = editor.isEmpty` here was
		// removed after a mutation showed it inert — the suite asserts the
		// button becomes enabled, so if a future bump flips that default the
		// test goes red and says so, which a defensive line would have hidden.
		editor.chain().setContent(next).focus('end').run();
		return true;
	}

	async function doSubmit() {
		if (busy || !editor) return;
		const md = currentMarkdown();
		if (md === '') return;
		// Capture the composer's item identity BEFORE the await. This composer
		// is REUSED across a no-{#key} item switch in the timeline (its `itemId`
		// prop just changes), so if the user switches A→B while A's submit is in
		// flight, clearing on completion would ERASE B's freshly-typed draft
		// (PLAN-2105 / TASK-2112; Codex). Only clear when the composer is still
		// on the same item and its editor is still alive.
		const reqWs = wsSlug;
		const reqItem = itemId;
		saving = true;
		try {
			await onSubmit(md);
			// Composer behaviour: clear on success. In edit/reply mode the host
			// unmounts this component, so the clear is harmless there.
			//
			// ...but only if the composer still holds what was SENT. Anything
			// added during the round trip is not part of the submitted comment,
			// and clearing it destroys it: a quote pushed in through
			// `appendMarkdown` while the submit was in flight (IDEA-2843, codex
			// round 5), and — pre-existing, same mechanism — anything the user
			// typed. This is the same class as the item-identity capture above,
			// applied to the CONTENT rather than to which item it belongs to.
			if (
				editor &&
				!editor.isDestroyed &&
				reqWs === wsSlug &&
				reqItem === itemId &&
				currentMarkdown() === md
			) {
				editor.commands.clearContent();
			}
		} catch {
			// Keep the draft so the user can retry.
		} finally {
			saving = false;
		}
	}

	/**
	 * Reads the CURRENT host address at emit time (PLAN-2392 DR-8).
	 *
	 * This composer is reused across a no-{#key} item switch — the same reason
	 * `doSubmit` above captures its item before awaiting — so a value baked
	 * into the extension config at mount would address the PREVIOUS item after
	 * a switch, and the host would correctly ignore the event. Tiptap's
	 * `options` getter returns a fresh spread per access, so there is no
	 * writing the new value in afterwards either (see hostAddress.ts).
	 */
	const readHostAddress = (): AttachmentHostAddress => ({
		workspaceSlug: wsSlug,
		itemId: itemId ?? '',
		hostToken
	});

	const attachmentUrl = (uuid: string, variant?: 'thumb-sm' | 'thumb-md' | 'original') =>
		wsSlug ? api.attachments.downloadUrl(wsSlug, uuid, variant) : `pad-attachment:${uuid}`;

	onMount(() => {
		if (!element) return;

		editor = new Editor({
			element,
			content,
			autofocus: autofocus ? 'end' : false,
			extensions: [
				StarterKit.configure({ link: false }),
				Link.configure({ openOnClick: false, autolink: true, linkOnPaste: true }),
				Placeholder.configure({ placeholder }),
				Markdown.configure({ html: true, transformPastedText: true, transformCopiedText: true }),
				AttachmentImage.configure({
					getDownloadUrl: attachmentUrl,
					workspaceSlug: wsSlug,
					// Panel / viewer addressing (PLAN-2392 DR-8).
					address: readHostAddress,
					// Rotate/crop stays DISABLED in comments — keep it lean. This
					// reader is deliberately constant: the body editor wires its
					// reader to the server's capabilities (BUG-2426), and the
					// comment composer must not follow it there.
					supportedFormats: () => [],
					transform: async () => {
						throw new Error('Image transforms are not available in comments.');
					}
				}),
				AttachmentChip.configure({
					getDownloadUrl: attachmentUrl,
					workspaceSlug: wsSlug,
					address: readHostAddress
				}),
				AttachmentUpload.configure({
					// Wrap upload so the host can track in-flight uploads and gate
					// submit — the plugin doesn't expose its placeholder count.
					upload: async (file) => {
						if (!wsSlug) {
							throw new Error('No workspace context — open a comment inside a workspace to attach files.');
						}
						pendingUploads += 1;
						try {
							const uploadItemId = itemId;
							const result = await api.attachments.upload(wsSlug, file, uploadItemId);
							// A comment upload carries item context too, so the
							// server associates it and it belongs in that item's
							// attachment strip — announce it like the body editor
							// does, or the strip stays stale until reload
							// (PLAN-2382 / TASK-2385).
							notifyAttachmentUploaded(uploadItemId, toUploadedAttachment(result));
							return result;
						} finally {
							pendingUploads -= 1;
						}
					},
					onError: (filename, message) => {
						console.error(`[comment attachment] ${filename}: ${message}`);
						if (typeof window !== 'undefined' && typeof window.alert === 'function') {
							window.alert(`Couldn't upload ${filename}: ${message}`);
						}
					}
				})
			],
			editorProps: {
				handleKeyDown: (_view, event) => {
					if ((event.metaKey || event.ctrlKey) && event.key === 'Enter') {
						event.preventDefault();
						doSubmit();
						return true;
					}
					if (event.key === 'Escape' && onCancel) {
						event.preventDefault();
						onCancel();
						return true;
					}
					return false;
				}
			},
			onUpdate: ({ editor: e }) => {
				empty = e.isEmpty;
			}
		});
		empty = editor.isEmpty;
	});

	onDestroy(() => {
		editor?.destroy();
	});
</script>

<div class="comment-editor" class:busy>
	<div class="ce-surface prose" bind:this={element}></div>
	<div class="ce-actions">
		<span class="ce-hint">
			{#if pendingUploads > 0}
				Uploading {pendingUploads} file{pendingUploads === 1 ? '' : 's'}…
			{:else}
				{onCancel ? 'Ctrl+Enter to submit · Esc to cancel' : 'Ctrl+Enter to submit · paste or drop an image'}
			{/if}
		</span>
		<div class="ce-buttons">
			{#if onCancel}
				<button class="ce-cancel" type="button" onclick={onCancel} disabled={saving}>Cancel</button>
			{/if}
			<button class="ce-submit" type="button" onclick={doSubmit} disabled={busy || empty}>
				{saving || submitting ? 'Posting…' : submitLabel}
			</button>
		</div>
	</div>
</div>

<style>
	.comment-editor {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}

	.ce-surface {
		width: 100%;
		min-height: 60px;
		max-height: 360px;
		overflow-y: auto;
		padding: var(--space-2) var(--space-3);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-primary);
		font-size: 0.9em;
		line-height: 1.5;
	}

	.ce-surface :global(.ProseMirror) {
		outline: none;
		min-height: 44px;
	}

	/* Placeholder (Placeholder extension renders a data-attr on the empty doc). */
	.ce-surface :global(.ProseMirror p.is-editor-empty:first-child::before) {
		content: attr(data-placeholder);
		color: var(--text-muted);
		float: left;
		height: 0;
		pointer-events: none;
	}

	.ce-surface :global(p:first-child) {
		margin-top: 0;
	}
	.ce-surface :global(p:last-child) {
		margin-bottom: 0;
	}

	/* Inline image previews render as compact thumbnails while composing,
	   matching the rendered-comment display. */
	.ce-surface :global(img[data-attachment-id]) {
		max-width: 280px;
		max-height: 180px;
		width: auto;
		height: auto;
		object-fit: contain;
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
	}

	.ce-surface:focus-within {
		border-color: var(--accent-blue);
	}

	.comment-editor.busy .ce-surface {
		opacity: 0.85;
	}

	.ce-actions {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-2);
	}

	.ce-hint {
		font-size: 0.75em;
		color: var(--text-muted);
	}

	.ce-buttons {
		display: flex;
		gap: var(--space-2);
	}

	.ce-submit {
		padding: var(--space-1) var(--space-4);
		background: var(--accent-blue);
		border: none;
		border-radius: var(--radius);
		color: #fff;
		font-size: 0.85em;
		font-weight: 500;
		cursor: pointer;
	}

	.ce-submit:hover:not(:disabled) {
		filter: brightness(1.1);
	}

	.ce-submit:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}

	.ce-cancel {
		padding: var(--space-1) var(--space-3);
		background: transparent;
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-secondary);
		font-size: 0.85em;
		cursor: pointer;
	}

	.ce-cancel:hover:not(:disabled) {
		background: var(--bg-tertiary);
	}
</style>
