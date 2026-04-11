<script lang="ts">
	import { onDestroy } from 'svelte';
	import { api } from '$lib/api/client';
	import { sseService } from '$lib/services/sse.svelte';
	import { relativeTime, renderMarkdown } from '$lib/utils/markdown';
	import type { Comment, Item } from '$lib/types';

	interface Props {
		wsSlug: string;
		username?: string;
		itemSlug: string;
		items?: Item[];
	}

	let { wsSlug, username = '', itemSlug, items = [] }: Props = $props();

	let comments = $state<Comment[]>([]);
	let loading = $state(true);
	let error = $state('');
	let newBody = $state('');
	let submitting = $state(false);
	let deletingId = $state<string | null>(null);

	async function loadComments() {
		try {
			const result = await api.comments.list(wsSlug, itemSlug);
			comments = result.sort(
				(a, b) => new Date(a.created_at).getTime() - new Date(b.created_at).getTime()
			);
		} catch (err) {
			error = err instanceof Error ? err.message : 'Failed to load comments';
		} finally {
			loading = false;
		}
	}

	async function handleSubmit() {
		const body = newBody.trim();
		if (!body || submitting) return;

		submitting = true;
		try {
			await api.comments.create(wsSlug, itemSlug, {
				body,
				created_by: 'user',
				source: 'web'
			});
			newBody = '';
			await loadComments();
		} catch (err) {
			error = err instanceof Error ? err.message : 'Failed to post comment';
		} finally {
			submitting = false;
		}
	}

	async function handleDelete(commentId: string) {
		if (!confirm('Delete this comment?')) return;

		deletingId = commentId;
		try {
			await api.comments.delete(wsSlug, commentId);
			await loadComments();
		} catch (err) {
			error = err instanceof Error ? err.message : 'Failed to delete comment';
		} finally {
			deletingId = null;
		}
	}

	function handleKeydown(e: KeyboardEvent) {
		if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
			e.preventDefault();
			handleSubmit();
		}
	}

	function authorLabel(createdBy: string): string {
		return createdBy === 'agent' ? 'Agent' : 'User';
	}

	function sourceLabel(source: string): string {
		const labels: Record<string, string> = {
			cli: 'CLI',
			web: 'Web',
			skill: 'Skill'
		};
		return labels[source] ?? source;
	}

	$effect(() => {
		// Re-fetch when workspace or item changes
		void wsSlug;
		void itemSlug;
		loading = true;
		error = '';
		loadComments();
	});

	const unsubscribe = sseService.onItemEvent((event) => {
		if (
			(event.type === 'comment_created' || event.type === 'comment_deleted') &&
			event.item_id
		) {
			loadComments();
		}
	});

	onDestroy(() => {
		unsubscribe();
	});
</script>

<div class="comment-thread">
	<div class="thread-header">
		<h4>Comments <span class="count">({comments.length})</span></h4>
	</div>

	{#if loading}
		<div class="loading">
			<span class="spinner"></span>
			<span>Loading comments...</span>
		</div>
	{:else if error}
		<div class="error-msg">{error}</div>
	{:else}
		<div class="comments-list">
			{#each comments as comment (comment.id)}
				<div class="comment" class:deleting={deletingId === comment.id}>
					<div class="comment-meta">
						<span
							class="author-badge"
							class:author-user={comment.created_by !== 'agent'}
							class:author-agent={comment.created_by === 'agent'}
						>
							{authorLabel(comment.created_by)}
						</span>
						<span class="source">{sourceLabel(comment.source)}</span>
						<span class="timestamp" title={new Date(comment.created_at).toLocaleString()}>{relativeTime(comment.created_at)}</span>
						<button
							class="delete-btn"
							type="button"
							title="Delete comment"
							disabled={deletingId === comment.id}
							onclick={() => handleDelete(comment.id)}
						>
							&#10005;
						</button>
					</div>
					<div class="comment-body prose">
						{@html renderMarkdown(comment.body, items, wsSlug, username)}
					</div>
				</div>
			{:else}
				<p class="empty">No comments yet.</p>
			{/each}
		</div>
	{/if}

	<form class="compose" onsubmit={(e) => { e.preventDefault(); handleSubmit(); }}>
		<textarea
			class="compose-input"
			placeholder="Write a comment..."
			rows="3"
			bind:value={newBody}
			onkeydown={handleKeydown}
			disabled={submitting}
		></textarea>
		<div class="compose-actions">
			<span class="hint">Ctrl+Enter to submit</span>
			<button
				class="submit-btn"
				type="submit"
				disabled={submitting || !newBody.trim()}
			>
				{submitting ? 'Posting...' : 'Comment'}
			</button>
		</div>
	</form>
</div>

<style>
	/* ── Container ──────────────────────────────────────────────────────────── */

	.comment-thread {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}

	/* ── Header ────────────────────────────────────────────────────────────── */

	.thread-header {
		display: flex;
		align-items: center;
	}

	.thread-header h4 {
		margin: 0;
		font-size: 0.95em;
		font-weight: 600;
		color: var(--text-primary);
	}

	.count {
		font-weight: 400;
		color: var(--text-muted);
		font-size: 0.9em;
	}

	/* ── Loading ────────────────────────────────────────────────────────────── */

	.loading {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-4) 0;
		color: var(--text-muted);
		font-size: 0.9em;
		justify-content: center;
	}

	.spinner {
		width: 16px;
		height: 16px;
		border: 2px solid var(--border);
		border-top-color: var(--accent-blue);
		border-radius: 50%;
		animation: spin 0.6s linear infinite;
	}

	@keyframes spin {
		to { transform: rotate(360deg); }
	}

	/* ── Error ──────────────────────────────────────────────────────────────── */

	.error-msg {
		padding: var(--space-2) var(--space-3);
		background: color-mix(in srgb, var(--accent-red, #ef4444) 12%, transparent);
		color: var(--accent-red, #ef4444);
		border-radius: var(--radius);
		font-size: 0.85em;
	}

	/* ── Comments list ─────────────────────────────────────────────────────── */

	.comments-list {
		display: flex;
		flex-direction: column;
	}

	.comment {
		padding: var(--space-3) 0;
		border-bottom: 1px solid var(--border);
	}

	.comment:last-child {
		border-bottom: none;
	}

	.comment.deleting {
		opacity: 0.5;
		pointer-events: none;
	}

	.empty {
		color: var(--text-muted);
		font-size: 0.9em;
		margin: 0;
		padding: var(--space-3) 0;
	}

	/* ── Comment meta ──────────────────────────────────────────────────────── */

	.comment-meta {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		margin-bottom: var(--space-2);
	}

	.author-badge {
		display: inline-flex;
		align-items: center;
		padding: 1px var(--space-2);
		border-radius: 9999px;
		font-size: 0.7em;
		font-weight: 500;
		text-transform: uppercase;
		letter-spacing: 0.03em;
		line-height: 1.6;
	}

	.author-user {
		background: color-mix(in srgb, var(--accent-blue) 15%, transparent);
		color: var(--accent-blue);
	}

	.author-agent {
		background: color-mix(in srgb, var(--accent-purple, #a855f7) 15%, transparent);
		color: var(--accent-purple, #a855f7);
	}

	.source {
		font-size: 0.75em;
		color: var(--text-muted);
	}

	.timestamp {
		font-size: 0.75em;
		color: var(--text-muted);
	}

	.delete-btn {
		margin-left: auto;
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 0.75em;
		cursor: pointer;
		padding: var(--space-1);
		border-radius: var(--radius-sm);
		line-height: 1;
		opacity: 0;
		transition: opacity 0.15s;
	}

	.comment:hover .delete-btn {
		opacity: 1;
	}

	.delete-btn:hover {
		color: var(--accent-red, #ef4444);
		background: color-mix(in srgb, var(--accent-red, #ef4444) 10%, transparent);
	}

	.delete-btn:disabled {
		opacity: 0.3;
		cursor: not-allowed;
	}

	/* ── Comment body ──────────────────────────────────────────────────────── */

	.comment-body {
		font-size: 0.9em;
		color: var(--text-primary);
		line-height: 1.5;
	}

	.comment-body :global(p) {
		margin: 0 0 var(--space-2) 0;
	}

	.comment-body :global(p:last-child) {
		margin-bottom: 0;
	}

	.comment-body :global(code) {
		font-family: var(--font-mono);
		font-size: 0.9em;
		padding: 1px var(--space-1);
		background: var(--bg-tertiary);
		border-radius: var(--radius-sm);
	}

	.comment-body :global(pre) {
		background: var(--bg-tertiary);
		padding: var(--space-2) var(--space-3);
		border-radius: var(--radius);
		overflow-x: auto;
		margin: var(--space-2) 0;
	}

	.comment-body :global(pre code) {
		padding: 0;
		background: none;
	}

	.comment-body :global(a) {
		color: var(--accent-blue);
		text-decoration: none;
	}

	.comment-body :global(a:hover) {
		text-decoration: underline;
	}

	/* ── Compose ────────────────────────────────────────────────────────────── */

	.compose {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		padding-top: var(--space-2);
		border-top: 1px solid var(--border);
	}

	.compose-input {
		width: 100%;
		padding: var(--space-2) var(--space-3);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-primary);
		font-size: 0.9em;
		font-family: inherit;
		line-height: 1.5;
		resize: vertical;
		min-height: 60px;
	}

	.compose-input::placeholder {
		color: var(--text-muted);
	}

	.compose-input:focus {
		outline: none;
		border-color: var(--accent-blue);
	}

	.compose-input:disabled {
		opacity: 0.6;
	}

	.compose-actions {
		display: flex;
		align-items: center;
		justify-content: space-between;
	}

	.hint {
		font-size: 0.75em;
		color: var(--text-muted);
	}

	.submit-btn {
		padding: var(--space-1) var(--space-4);
		background: var(--accent-blue);
		border: none;
		border-radius: var(--radius);
		color: #fff;
		font-size: 0.85em;
		font-weight: 500;
		cursor: pointer;
	}

	.submit-btn:hover:not(:disabled) {
		filter: brightness(1.1);
	}

	.submit-btn:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}
</style>
