<script lang="ts">
	import { onMount, onDestroy } from 'svelte';
	import { api } from '$lib/api/client';
	import { sseService } from '$lib/services/sse.svelte';
	import type { TimelineEntry, TimelineResponse, Item } from '$lib/types';
	import TimelineCommentCard from './TimelineCommentCard.svelte';
	import TimelineActivityCard from './TimelineActivityCard.svelte';
	import TimelineVersionCard from './TimelineVersionCard.svelte';

	interface Props {
		wsSlug: string;
		itemSlug: string;
		currentContent: string;
		items?: Item[];
		onRestore?: (item: Item) => void;
	}

	let { wsSlug, itemSlug, currentContent, items = [], onRestore }: Props = $props();

	let entries: TimelineEntry[] = $state([]);
	let hasMore: boolean = $state(false);
	let loading: boolean = $state(false);
	let loadingMore: boolean = $state(false);
	let error: string = $state('');
	let newBody: string = $state('');

	// Current user ID for reaction toggle (fetched from auth session).
	let currentUserId: string = $state('');

	onMount(async () => {
		try {
			const session = await api.auth.session();
			if (session.user?.id) {
				currentUserId = session.user.id;
			}
		} catch {
			// Auth may not be configured — proceed without user ID.
		}
	});

	async function loadTimeline() {
		loading = true;
		error = '';
		try {
			const resp: TimelineResponse = await api.timeline.list(wsSlug, itemSlug);
			entries = resp.entries;
			hasMore = resp.has_more;
		} catch (err: any) {
			error = err?.message ?? 'Failed to load timeline';
		} finally {
			loading = false;
		}
	}

	async function loadMore() {
		if (loadingMore || entries.length === 0) return;
		const oldest = entries[entries.length - 1];
		loadingMore = true;
		try {
			const resp: TimelineResponse = await api.timeline.list(wsSlug, itemSlug, {
				before: oldest.created_at
			});
			// Deduplicate by ID to handle boundary overlap from <= queries.
			const existingIds = new Set(entries.map((e) => e.id));
			const newEntries = resp.entries.filter((e) => !existingIds.has(e.id));
			entries = [...entries, ...newEntries];
			hasMore = resp.has_more;
		} catch (err: any) {
			error = err?.message ?? 'Failed to load more';
		} finally {
			loadingMore = false;
		}
	}

	$effect(() => {
		void wsSlug;
		void itemSlug;
		loadTimeline();
	});

	const relevantEvents = new Set([
		'item_updated',
		'comment_created',
		'comment_deleted',
		'reaction_added',
		'reaction_removed'
	]);

	const unsubscribe = sseService.onItemEvent(async (event) => {
		if (relevantEvents.has(event.type)) {
			// Fetch only the newest entries and prepend, preserving paginated state.
			try {
				const resp: TimelineResponse = await api.timeline.list(wsSlug, itemSlug);
				const existingIds = new Set(entries.map((e) => e.id));
				const newEntries = resp.entries.filter((e) => !existingIds.has(e.id));
				if (newEntries.length > 0) {
					entries = [...newEntries, ...entries];
				}
				// Also update existing entries (e.g., reaction changes on existing comments).
				const newById = new Map(resp.entries.map((e) => [e.id, e]));
				entries = entries.map((e) => newById.get(e.id) ?? e);
			} catch {
				// Silently ignore SSE refresh failures.
			}
		}
	});

	onDestroy(() => {
		unsubscribe();
	});

	let submitting: boolean = $state(false);

	async function submitComment() {
		if (!newBody.trim() || submitting) return;
		submitting = true;
		try {
			await api.comments.create(wsSlug, itemSlug, {
				body: newBody.trim(),
				created_by: 'user',
				source: 'web'
			});
			newBody = '';
			await loadTimeline();
		} catch (err: any) {
			error = err?.message ?? 'Failed to post comment';
		} finally {
			submitting = false;
		}
	}

	function handleKeydown(e: KeyboardEvent) {
		if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
			e.preventDefault();
			submitComment();
		}
	}

	async function handleReply(commentId: string, body: string) {
		try {
			await api.comments.reply(wsSlug, commentId, {
				body,
				created_by: 'user',
				source: 'web'
			});
			await loadTimeline();
		} catch (err: any) {
			error = err?.message ?? 'Failed to post reply';
		}
	}

	async function handleDelete(commentId: string) {
		if (!confirm('Delete this comment?')) return;
		try {
			await api.comments.delete(wsSlug, commentId);
			await loadTimeline();
		} catch (err: any) {
			error = err?.message ?? 'Failed to delete comment';
		}
	}

	async function handleReaction(commentId: string, emoji: string) {
		try {
			await api.comments.addReaction(wsSlug, commentId, emoji);
			await loadTimeline();
		} catch (err: any) {
			error = err?.message ?? 'Failed to add reaction';
		}
	}

	async function handleRemoveReaction(commentId: string, emoji: string) {
		try {
			await api.comments.removeReaction(wsSlug, commentId, emoji);
			await loadTimeline();
		} catch (err: any) {
			error = err?.message ?? 'Failed to remove reaction';
		}
	}

	function dotClass(kind: TimelineEntry['kind']): string {
		if (kind === 'comment') return 'dot-comment';
		if (kind === 'version') return 'dot-version';
		return 'dot-activity';
	}
</script>

<section class="timeline">
	<header class="timeline-header">
		<h3 class="timeline-title">Timeline</h3>
		{#if entries.length > 0}
			<span class="entry-count">{entries.length}{hasMore ? '+' : ''}</span>
		{/if}
	</header>

	<!-- Comment compose -->
	<div class="compose">
		<textarea
			class="compose-input"
			placeholder="Write a comment..."
			bind:value={newBody}
			onkeydown={handleKeydown}
			disabled={submitting}
		></textarea>
		<div class="compose-actions">
			<span class="shortcut-hint">Ctrl+Enter to submit</span>
			<button
				class="submit-btn"
				type="button"
				disabled={!newBody.trim() || submitting}
				onclick={submitComment}
			>
				{submitting ? 'Posting...' : 'Comment'}
			</button>
		</div>
	</div>

	{#if loading && entries.length === 0}
		<div class="loading">
			<span class="spinner"></span>
			<span class="loading-text">Loading timeline...</span>
		</div>
	{/if}

	{#if error}
		<div class="error">{error}</div>
	{/if}

	{#if !loading || entries.length > 0}
		<div class="entry-list">
			{#each entries as entry (entry.id)}
				<div class="entry">
					<div class="entry-rail">
						<span class="dot {dotClass(entry.kind)}"></span>
						<span class="line"></span>
					</div>
					<div class="entry-content">
						{#if entry.kind === 'comment' && entry.comment}
							<TimelineCommentCard
								comment={entry.comment}
								{wsSlug}
								{items}
								{currentUserId}
								onDelete={handleDelete}
								onReply={handleReply}
								onReaction={handleReaction}
								onRemoveReaction={handleRemoveReaction}
							/>
						{:else if entry.kind === 'activity' && entry.activity}
							<TimelineActivityCard activity={entry.activity} />
						{:else if entry.kind === 'version' && entry.version}
							<TimelineVersionCard
								version={entry.version}
								{wsSlug}
								{itemSlug}
								{currentContent}
								{onRestore}
							/>
						{/if}
					</div>
				</div>
			{/each}

			{#if entries.length === 0 && !loading}
				<div class="empty">No timeline entries yet.</div>
			{/if}
		</div>

		{#if hasMore}
			<button class="load-more-btn" type="button" disabled={loadingMore} onclick={loadMore}>
				{loadingMore ? 'Loading...' : 'Load more'}
			</button>
		{/if}
	{/if}
</section>

<style>
	.timeline {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}

	.timeline-header {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}

	.timeline-title {
		margin: 0;
		font-size: 1em;
		font-weight: 600;
		color: var(--text-primary);
	}

	.entry-count {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		min-width: 1.5em;
		padding: 0 var(--space-1);
		background: var(--bg-tertiary);
		border-radius: 9999px;
		font-size: 0.75em;
		font-weight: 600;
		color: var(--text-muted);
		line-height: 1.6;
	}

	/* ── Compose ──────────────────────────────────────────────────────────── */

	.compose {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
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
		justify-content: flex-end;
		gap: var(--space-3);
	}

	.shortcut-hint {
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

	/* ── Loading / Error ──────────────────────────────────────────────────── */

	.loading {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-4);
		justify-content: center;
		color: var(--text-muted);
	}

	.spinner {
		display: inline-block;
		width: 16px;
		height: 16px;
		border: 2px solid var(--border);
		border-top-color: var(--accent-blue);
		border-radius: 50%;
		animation: spin 0.6s linear infinite;
	}

	@keyframes spin {
		to {
			transform: rotate(360deg);
		}
	}

	.loading-text {
		font-size: 0.85em;
	}

	.error {
		padding: var(--space-2) var(--space-3);
		background: color-mix(in srgb, var(--accent-red, #ef4444) 10%, transparent);
		border: 1px solid color-mix(in srgb, var(--accent-red, #ef4444) 30%, transparent);
		border-radius: var(--radius);
		color: var(--accent-red, #ef4444);
		font-size: 0.85em;
	}

	/* ── Timeline entries ─────────────────────────────────────────────────── */

	.entry-list {
		display: flex;
		flex-direction: column;
	}

	.entry {
		display: flex;
		gap: var(--space-3);
	}

	.entry-rail {
		display: flex;
		flex-direction: column;
		align-items: center;
		flex-shrink: 0;
		width: 16px;
		padding-top: var(--space-2);
	}

	.dot {
		width: 10px;
		height: 10px;
		border-radius: 50%;
		flex-shrink: 0;
		z-index: 1;
	}

	.dot-comment {
		background: var(--accent-blue);
	}

	.dot-activity {
		background: var(--text-muted);
	}

	.dot-version {
		background: var(--accent-green);
	}

	.line {
		width: 1px;
		flex: 1;
		background: var(--border);
	}

	.entry:last-child .line {
		display: none;
	}

	.entry-content {
		flex: 1;
		min-width: 0;
		padding-bottom: var(--space-3);
	}

	.empty {
		text-align: center;
		padding: var(--space-6);
		color: var(--text-muted);
		font-size: 0.9em;
	}

	.load-more-btn {
		display: block;
		width: 100%;
		padding: var(--space-2) var(--space-4);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-muted);
		font-size: 0.85em;
		font-weight: 500;
		cursor: pointer;
		text-align: center;
	}

	.load-more-btn:hover:not(:disabled) {
		color: var(--text-primary);
		border-color: var(--accent-blue);
	}

	.load-more-btn:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}
</style>
