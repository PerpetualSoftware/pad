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
	import { unescapeDocLinks } from '$lib/utils/markdown';
	import { AttachmentImage } from './editor/attachment-image';
	import { AttachmentChip } from './editor/attachment-chip';
	import { AttachmentUpload } from './editor/attachment-upload';

	interface Props {
		/** Initial markdown body. Parsed as markdown on mount. */
		content?: string;
		placeholder?: string;
		/** Workspace slug — required for attachment upload + image URLs. */
		wsSlug: string;
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

	async function doSubmit() {
		if (busy || !editor) return;
		const md = currentMarkdown();
		if (md === '') return;
		saving = true;
		try {
			await onSubmit(md);
			// Composer behaviour: clear on success. In edit/reply mode the host
			// unmounts this component, so the clear is harmless there.
			editor.commands.clearContent();
		} catch {
			// Keep the draft so the user can retry.
		} finally {
			saving = false;
		}
	}

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
					// Rotate/crop stays disabled in comments — keep it lean.
					supportedFormats: [] as string[],
					transform: async () => {
						throw new Error('Image transforms are not available in comments.');
					}
				}),
				AttachmentChip.configure({ getDownloadUrl: attachmentUrl, workspaceSlug: wsSlug }),
				AttachmentUpload.configure({
					// Wrap upload so the host can track in-flight uploads and gate
					// submit — the plugin doesn't expose its placeholder count.
					upload: async (file) => {
						if (!wsSlug) {
							throw new Error('No workspace context — open a comment inside a workspace to attach files.');
						}
						pendingUploads += 1;
						try {
							return await api.attachments.upload(wsSlug, file);
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
