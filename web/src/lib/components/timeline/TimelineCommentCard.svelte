<script lang="ts">
	import type { Comment, Item, Reaction } from '$lib/types';
	import { relativeTime, renderMarkdown } from '$lib/utils/markdown';
	import ReactionPicker from './ReactionPicker.svelte';

	interface Props {
		comment: Comment;
		wsSlug: string;
		username?: string;
		items: Item[];
		currentUserId?: string;
		onDelete: (commentId: string) => void;
		onReply: (commentId: string, body: string) => void | Promise<void>;
		onReaction: (commentId: string, emoji: string) => void;
		onRemoveReaction: (commentId: string, emoji: string) => void;
	}

	let { comment, wsSlug, username = '', items, currentUserId = '', onDelete, onReply, onReaction, onRemoveReaction }: Props = $props();

	let showReplyForm = $state(false);
	let replyBody = $state('');
	let submittingReply = $state(false);

	async function submitReply() {
		const body = replyBody.trim();
		if (!body || submittingReply) return;
		submittingReply = true;
		try {
			await onReply(comment.id, body);
			replyBody = '';
			showReplyForm = false;
		} catch {
			// Keep draft on failure so the user can retry.
		} finally {
			submittingReply = false;
		}
	}

	function handleReplyKeydown(e: KeyboardEvent) {
		if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
			e.preventDefault();
			submitReply();
		}
		if (e.key === 'Escape') {
			showReplyForm = false;
			replyBody = '';
		}
	}

	interface ReactionGroup {
		emoji: string;
		count: number;
		actors: string[];
	}

	function groupReactions(reactions: Reaction[] | undefined): ReactionGroup[] {
		if (!reactions || reactions.length === 0) return [];
		const map = new Map<string, ReactionGroup>();
		for (const r of reactions) {
			const existing = map.get(r.emoji);
			if (existing) {
				existing.count++;
				existing.actors.push(r.actor_name ?? r.actor);
			} else {
				map.set(r.emoji, { emoji: r.emoji, count: 1, actors: [r.actor_name ?? r.actor] });
			}
		}
		return Array.from(map.values());
	}

	function getActorLabel(createdBy: string): string {
		return createdBy === 'agent' ? 'Agent' : 'User';
	}

	function getActorBadgeClass(createdBy: string): string {
		return createdBy === 'agent' ? 'author-agent' : 'author-user';
	}

	function getBorderClass(createdBy: string): string {
		return createdBy === 'agent' ? 'border-agent' : 'border-user';
	}

	function getSourceLabel(source: string): string {
		const labels: Record<string, string> = {
			cli: 'CLI',
			web: 'Web',
			skill: 'Skill'
		};
		return labels[source] ?? source;
	}

	function hasMyReaction(reactions: Reaction[] | undefined, emoji: string): boolean {
		if (!reactions || !currentUserId) return false;
		return reactions.some((r) => r.emoji === emoji && r.user_id === currentUserId);
	}

	function toggleReaction(emoji: string) {
		if (hasMyReaction(comment.reactions, emoji)) {
			onRemoveReaction(comment.id, emoji);
		} else {
			onReaction(comment.id, emoji);
		}
	}

	function handleAddReaction(emoji: string) {
		// From the picker — always add.
		onReaction(comment.id, emoji);
	}

	function toggleReplyReaction(reply: Comment, emoji: string) {
		if (hasMyReaction(reply.reactions, emoji)) {
			onRemoveReaction(reply.id, emoji);
		} else {
			onReaction(reply.id, emoji);
		}
	}

	function handleAddReplyReaction(reply: Comment, emoji: string) {
		onReaction(reply.id, emoji);
	}

	const reactionGroups = $derived(groupReactions(comment.reactions));
</script>

<div class="comment-card {getBorderClass(comment.created_by)}">
	<div class="comment-header">
		<span class="author-badge {getActorBadgeClass(comment.created_by)}">{getActorLabel(comment.created_by)}</span>
		{#if comment.author}
			<span class="author-name">{comment.author}</span>
		{/if}
		<span class="source-badge">{getSourceLabel(comment.source)}</span>
		<span class="spacer"></span>
		<span class="timestamp" title={new Date(comment.created_at).toLocaleString()}>{relativeTime(comment.created_at)}</span>
		<button
			class="delete-btn"
			type="button"
			onclick={() => onDelete(comment.id)}
			title="Delete comment"
		>
			<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
				<polyline points="3 6 5 6 21 6" />
				<path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2" />
			</svg>
		</button>
	</div>

	{#if comment.activity_id}
		<div class="activity-label">commented on update</div>
	{/if}

	<div class="comment-body markdown-body">
		{@html renderMarkdown(comment.body, items, wsSlug, username)}
	</div>

	<div class="comment-footer">
		<div class="reactions-row">
				{#each reactionGroups as group (group.emoji)}
					<button
						class="reaction-chip"
						class:mine={hasMyReaction(comment.reactions, group.emoji)}
						type="button"
						title={group.actors.join(', ')}
						onclick={() => toggleReaction(group.emoji)}
					>
						<span class="reaction-emoji">{group.emoji}</span>
						<span class="reaction-count">{group.count}</span>
					</button>
				{/each}
				<ReactionPicker onSelect={handleAddReaction} />
		</div>
		<button class="reply-btn" type="button" onclick={() => { showReplyForm = !showReplyForm; }}>
			Reply
		</button>
	</div>

	{#if showReplyForm}
		<div class="reply-compose">
			<textarea
				class="reply-input"
				placeholder="Write a reply..."
				bind:value={replyBody}
				onkeydown={handleReplyKeydown}
				disabled={submittingReply}
			></textarea>
			<div class="reply-actions">
				<span class="reply-hint">Ctrl+Enter to submit · Esc to cancel</span>
				<div class="reply-buttons">
					<button class="reply-cancel" type="button" onclick={() => { showReplyForm = false; replyBody = ''; }}>Cancel</button>
					<button class="reply-submit" type="button" disabled={!replyBody.trim() || submittingReply} onclick={submitReply}>
						{submittingReply ? 'Posting...' : 'Reply'}
					</button>
				</div>
			</div>
		</div>
	{/if}

	{#if comment.replies && comment.replies.length > 0}
		<div class="replies">
			{#each comment.replies as reply (reply.id)}
				{@const replyReactionGroups = groupReactions(reply.reactions)}
				<div class="reply-card {getBorderClass(reply.created_by)}">
					<div class="reply-header">
						<span class="author-badge {getActorBadgeClass(reply.created_by)}">{getActorLabel(reply.created_by)}</span>
						{#if reply.author}
							<span class="author-name">{reply.author}</span>
						{/if}
						<span class="spacer"></span>
						<span class="timestamp" title={new Date(reply.created_at).toLocaleString()}>{relativeTime(reply.created_at)}</span>
						<button
							class="delete-btn"
							type="button"
							onclick={() => onDelete(reply.id)}
							title="Delete reply"
						>
							<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
								<polyline points="3 6 5 6 21 6" />
								<path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2" />
							</svg>
						</button>
					</div>

					<div class="reply-body markdown-body">
						{@html renderMarkdown(reply.body, items, wsSlug, username)}
					</div>

						<div class="reactions-row">
							{#each replyReactionGroups as group (group.emoji)}
								<button
									class="reaction-chip"
									class:mine={hasMyReaction(reply.reactions, group.emoji)}
									type="button"
									title={group.actors.join(', ')}
									onclick={() => toggleReplyReaction(reply, group.emoji)}
								>
									<span class="reaction-emoji">{group.emoji}</span>
									<span class="reaction-count">{group.count}</span>
								</button>
							{/each}
							<ReactionPicker onSelect={(emoji) => handleAddReplyReaction(reply, emoji)} />
						</div>
				</div>
			{/each}
		</div>
	{/if}
</div>

<style>
	.comment-card {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		padding: var(--space-2) var(--space-3);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		border-left: 3px solid var(--accent-blue);
	}

	.comment-card.border-agent {
		border-left-color: var(--accent-purple, #a855f7);
	}

	.comment-card.border-user {
		border-left-color: var(--accent-blue);
	}

	.comment-header {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		flex-wrap: wrap;
		min-width: 0;
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

	.author-name {
		font-size: 0.85em;
		color: var(--text-primary);
		font-weight: 500;
	}

	.source-badge {
		display: inline-flex;
		align-items: center;
		padding: 0 var(--space-2);
		border-radius: var(--radius-sm);
		font-size: 0.75em;
		font-weight: 600;
		line-height: 1.75;
		white-space: nowrap;
		background: var(--bg-tertiary);
		color: var(--text-muted);
	}

	.spacer {
		flex: 1;
	}

	.timestamp {
		font-size: 0.8em;
		color: var(--text-muted);
		white-space: nowrap;
	}

	.delete-btn {
		display: flex;
		align-items: center;
		justify-content: center;
		padding: var(--space-1);
		border: none;
		background: none;
		color: var(--text-muted);
		cursor: pointer;
		border-radius: var(--radius-sm);
		opacity: 0;
		transition: opacity 0.15s, color 0.15s;
	}

	.comment-card:hover .comment-header > .delete-btn,
	.reply-card:hover .reply-header > .delete-btn {
		opacity: 1;
	}

	.delete-btn:hover {
		color: var(--accent-red);
		background: color-mix(in srgb, var(--accent-red) 10%, transparent);
	}

	.activity-label {
		font-size: 0.75em;
		color: var(--text-muted);
		font-style: italic;
	}

	.comment-body,
	.reply-body {
		font-size: 0.9em;
		line-height: 1.6;
		color: var(--text-primary);
		overflow-wrap: break-word;
	}

	.comment-body :global(p:last-child),
	.reply-body :global(p:last-child) {
		margin-bottom: 0;
	}

	.comment-body :global(p:first-child),
	.reply-body :global(p:first-child) {
		margin-top: 0;
	}

	.comment-footer {
		display: flex;
		align-items: center;
		gap: var(--space-3);
	}

	.reactions-row {
		display: flex;
		align-items: center;
		flex-wrap: wrap;
		gap: var(--space-1);
	}

	.reaction-chip {
		display: inline-flex;
		align-items: center;
		gap: 0.25em;
		padding: 2px var(--space-2);
		border: 1px solid var(--border);
		border-radius: 9999px;
		background: var(--bg-tertiary);
		cursor: pointer;
		font-size: 0.8em;
		line-height: 1.5;
	}

	.reaction-chip.mine {
		border-color: var(--accent-blue);
		background: color-mix(in srgb, var(--accent-blue) 12%, transparent);
	}

	.reaction-chip:hover {
		border-color: var(--accent-blue);
		background: color-mix(in srgb, var(--accent-blue) 10%, transparent);
	}

	.reaction-emoji {
		font-size: 1em;
	}

	.reaction-count {
		font-size: 0.85em;
		font-weight: 600;
		color: var(--text-muted);
	}

	.reply-btn {
		border: none;
		background: none;
		color: var(--text-muted);
		cursor: pointer;
		font-size: 0.8em;
		padding: 0;
		font-weight: 500;
	}

	.reply-btn:hover {
		color: var(--accent-blue);
	}

	.reply-compose {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}

	.reply-input {
		width: 100%;
		padding: var(--space-2) var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		color: var(--text-primary);
		font-size: 0.85em;
		font-family: inherit;
		line-height: 1.5;
		resize: vertical;
		min-height: 52px;
	}

	.reply-input::placeholder {
		color: var(--text-muted);
	}

	.reply-input:focus {
		outline: none;
		border-color: var(--accent-blue);
	}

	.reply-actions {
		display: flex;
		align-items: center;
		justify-content: space-between;
	}

	.reply-hint {
		font-size: 0.7em;
		color: var(--text-muted);
	}

	.reply-buttons {
		display: flex;
		gap: var(--space-2);
	}

	.reply-cancel {
		padding: var(--space-1) var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		background: var(--bg-secondary);
		color: var(--text-muted);
		font-size: 0.8em;
		cursor: pointer;
	}

	.reply-cancel:hover {
		color: var(--text-primary);
		border-color: var(--text-muted);
	}

	.reply-submit {
		padding: var(--space-1) var(--space-3);
		background: var(--accent-blue);
		border: none;
		border-radius: var(--radius-sm);
		color: #fff;
		font-size: 0.8em;
		font-weight: 500;
		cursor: pointer;
	}

	.reply-submit:hover:not(:disabled) {
		filter: brightness(1.1);
	}

	.reply-submit:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}

	.replies {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		margin-left: var(--space-3);
		padding-left: var(--space-3);
		border-left: 1px solid var(--border);
	}

	.reply-card {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
		padding: var(--space-2);
		background: var(--bg-tertiary);
		border-radius: var(--radius-sm);
		border-left: 2px solid var(--accent-blue);
	}

	.reply-card.border-agent {
		border-left-color: var(--accent-purple, #a855f7);
	}

	.reply-card.border-user {
		border-left-color: var(--accent-blue);
	}

	.reply-header {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		flex-wrap: wrap;
		min-width: 0;
	}
</style>
