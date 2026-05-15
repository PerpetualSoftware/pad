<script lang="ts">
	import type { Editor } from '@tiptap/core';
	import { marked } from 'marked';
	import { api, type ImportURLResponse } from '$lib/api/client';
	import { toastStore } from '$lib/stores/toast.svelte';

	interface Props {
		open: boolean;
		editor: Editor | null;
		onInserted?: (meta: ImportURLResponse) => void;
	}

	let { open = $bindable(), editor, onInserted }: Props = $props();

	// Working state for the modal. Reset when `open` transitions to true
	// so a re-opened modal starts fresh.
	let url = $state('');
	let isLoading = $state(false);
	let errorMessage = $state('');
	let result = $state<ImportURLResponse | null>(null);

	$effect(() => {
		if (open) {
			url = '';
			isLoading = false;
			errorMessage = '';
			result = null;
		}
	});

	function handleKeydown(e: KeyboardEvent) {
		if (open && e.key === 'Escape') {
			open = false;
		}
	}

	async function handleFetch() {
		errorMessage = '';
		result = null;
		const trimmed = url.trim();
		if (!trimmed) {
			errorMessage = 'Enter a URL.';
			return;
		}
		// Light client-side validation so obvious mistakes don't hit the
		// server. The server's SSRF + scheme guard is canonical.
		try {
			const parsed = new URL(trimmed);
			if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
				errorMessage = 'Only http(s) URLs are supported.';
				return;
			}
		} catch {
			errorMessage = 'That doesn’t look like a valid URL.';
			return;
		}

		isLoading = true;
		try {
			const resp = await api.importURL(trimmed);
			result = resp;
		} catch (err) {
			errorMessage = err instanceof Error ? err.message : 'Failed to fetch URL.';
		} finally {
			isLoading = false;
		}
	}

	function handleInsert() {
		if (!result || !editor) return;
		// The editor's tiptap-markdown extension handles paste-time
		// markdown decoding but `insertContent` accepts HTML directly,
		// so we convert markdown → HTML with `marked` (the project's
		// existing markdown renderer) and let TipTap parse the HTML
		// into ProseMirror nodes. This is the same shape the editor
		// uses on initial setContent, so structural fidelity is high.
		const html = marked.parse(result.markdown, { async: false }) as string;
		editor.chain().focus().insertContent(html).run();
		onInserted?.(result);
		toastStore.show(`Imported from ${result.source_url}`, 'success');
		open = false;
	}

	function handleCancel() {
		open = false;
	}
</script>

<svelte:window onkeydown={handleKeydown} />

{#if open}
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<!-- svelte-ignore a11y_no_static_element_interactions -->
	<div class="overlay" onclick={handleCancel}>
		<div class="modal" onclick={(e) => e.stopPropagation()}>
			<div class="modal-header">
				<h2>Insert from URL</h2>
				<button class="close-btn" type="button" onclick={handleCancel}>&#10005;</button>
			</div>

			<div class="modal-body">
				<p class="intro-copy">
					Paste a URL. Pad fetches the page server-side and converts the readable
					content (or OpenAPI spec) to markdown. Nothing is saved until you click
					<strong>Insert</strong>.
				</p>

				<form
					class="url-row"
					onsubmit={(e) => {
						e.preventDefault();
						handleFetch();
					}}
				>
					<!-- A modal opened from a deliberate click sets focus to its
						 primary input as expected behavior. Autofocus is the
						 simplest way to deliver that; the a11y warning's broader
						 concern (autofocus on page load) doesn't apply here
						 because the user actively triggered the modal. -->
					<!-- svelte-ignore a11y_autofocus -->
					<input
						type="url"
						placeholder="https://example.com/docs/page"
						bind:value={url}
						disabled={isLoading}
						autofocus
					/>
					<button type="submit" class="btn primary" disabled={isLoading || !url.trim()}>
						{isLoading ? 'Fetching…' : 'Fetch'}
					</button>
				</form>

				{#if errorMessage}
					<div class="error" role="alert">{errorMessage}</div>
				{/if}

				{#if result}
					<div class="result">
						<div class="result-meta">
							<span class="type-tag" class:openapi={result.detected_type === 'openapi'}>
								{result.detected_type === 'openapi' ? 'OpenAPI' : 'Generic'}
							</span>
							{#if result.title}
								<span class="title">{result.title}</span>
							{/if}
							<span class="source">{result.source_url}</span>
						</div>
						<pre class="preview">{result.markdown}</pre>
					</div>
				{/if}
			</div>

			<div class="modal-footer">
				<button type="button" class="btn" onclick={handleCancel}>Cancel</button>
				<button
					type="button"
					class="btn primary"
					disabled={!result || isLoading}
					onclick={handleInsert}
				>
					Insert at cursor
				</button>
			</div>
		</div>
	</div>
{/if}

<style>
	.overlay {
		position: fixed;
		inset: 0;
		background: rgba(0, 0, 0, 0.5);
		display: flex;
		align-items: center;
		justify-content: center;
		z-index: 1000;
	}
	.modal {
		background: var(--bg-primary);
		border-radius: var(--radius);
		box-shadow: 0 16px 48px rgba(0, 0, 0, 0.4);
		width: min(720px, 95vw);
		max-height: 90vh;
		display: flex;
		flex-direction: column;
	}
	.modal-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-4);
		border-bottom: 1px solid var(--border);
	}
	.modal-header h2 {
		margin: 0;
		font-size: 1.1rem;
	}
	.close-btn {
		background: transparent;
		border: none;
		font-size: 1.1rem;
		color: var(--text-secondary);
		cursor: pointer;
	}
	.modal-body {
		padding: var(--space-4);
		overflow-y: auto;
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	.intro-copy {
		color: var(--text-secondary);
		margin: 0;
		font-size: 0.9rem;
		line-height: 1.5;
	}
	.url-row {
		display: flex;
		gap: var(--space-2);
	}
	.url-row input {
		flex: 1;
		padding: var(--space-2) var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		font: inherit;
		color: var(--text-primary);
		background: var(--bg-secondary);
	}
	.url-row input:focus {
		outline: 2px solid var(--accent);
		outline-offset: -1px;
	}
	.btn {
		padding: var(--space-2) var(--space-3);
		border: 1px solid var(--border);
		background: var(--bg-secondary);
		border-radius: var(--radius-sm);
		font: inherit;
		color: var(--text-primary);
		cursor: pointer;
	}
	.btn.primary {
		background: var(--accent);
		color: var(--bg-primary);
		border-color: var(--accent);
	}
	.btn:disabled {
		opacity: 0.55;
		cursor: not-allowed;
	}
	.error {
		color: var(--error, #b91c1c);
		font-size: 0.9rem;
		background: color-mix(in srgb, var(--error, #b91c1c) 10%, transparent);
		padding: var(--space-2) var(--space-3);
		border-radius: var(--radius-sm);
	}
	.result {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.result-meta {
		display: flex;
		gap: var(--space-2);
		align-items: center;
		font-size: 0.85rem;
		color: var(--text-secondary);
		flex-wrap: wrap;
	}
	.type-tag {
		padding: 2px 8px;
		border-radius: 999px;
		background: var(--bg-secondary);
		font-weight: 600;
		font-size: 0.75rem;
		color: var(--text-primary);
	}
	.type-tag.openapi {
		background: color-mix(in srgb, var(--accent) 25%, var(--bg-secondary));
		color: var(--text-primary);
	}
	.title {
		font-weight: 600;
		color: var(--text-primary);
	}
	.source {
		font-family: var(--font-mono, monospace);
		font-size: 0.78rem;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		max-width: 100%;
	}
	.preview {
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		padding: var(--space-3);
		font-family: var(--font-mono, monospace);
		font-size: 0.85rem;
		line-height: 1.5;
		max-height: 360px;
		overflow: auto;
		white-space: pre-wrap;
		margin: 0;
	}
	.modal-footer {
		display: flex;
		justify-content: flex-end;
		gap: var(--space-2);
		padding: var(--space-3) var(--space-4);
		border-top: 1px solid var(--border);
	}
</style>
