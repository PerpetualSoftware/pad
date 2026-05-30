<script lang="ts">
	import type { Comment, Item, Reaction } from '$lib/types';
	import { relativeTime, renderMarkdown } from '$lib/utils/markdown';
	import type { AttachmentResolver } from '$lib/markdown/attachments';
	import CommentEditor from '$lib/components/CommentEditor.svelte';
	import ReactionPicker from './ReactionPicker.svelte';

	interface Props {
		comment: Comment;
		wsSlug: string;
		username?: string;
		items: Item[];
		currentUserId?: string;
		/**
		 * canEdit gates write affordances on this comment: reply button,
		 * reaction picker / per-reaction toggle, and delete (PLAN-1100 /
		 * TASK-1107). Existing reactions still render with counts so
		 * read-only viewers see who reacted; they just can't react
		 * themselves. Default true preserves behavior in callers that
		 * don't pass it.
		 */
		canEdit?: boolean;
		/**
		 * Resolves `pad-attachment:UUID` references in the comment / reply
		 * bodies to inline images or file chips (IDEA-1650). Optional —
		 * without it those references render as plain (broken) markdown
		 * links, the same graceful degradation renderMarkdown uses elsewhere.
		 */
		attachmentResolver?: AttachmentResolver;
		/** True when the current user is a platform admin (can edit any comment). */
		isAdmin?: boolean;
		onDelete: (commentId: string) => void;
		onReply: (commentId: string, body: string) => void | Promise<void>;
		/** Edits a comment/reply body. Should throw on failure so the editor keeps the draft. */
		onEdit: (commentId: string, body: string) => void | Promise<void>;
		onReaction: (commentId: string, emoji: string) => void;
		onRemoveReaction: (commentId: string, emoji: string) => void;
	}

	let { comment, wsSlug, username = '', items, currentUserId = '', canEdit = true, attachmentResolver, isAdmin = false, onDelete, onReply, onEdit, onReaction, onRemoveReaction }: Props = $props();

	let showReplyForm = $state(false);
	let submittingReply = $state(false);

	// Inline edit state. editing = the top-level comment; editingReplyId = a
	// specific reply (only one open at a time).
	let editing = $state(false);
	let editingReplyId = $state<string | null>(null);

	// A comment is editable by its author or a platform admin (TASK-1665) —
	// distinct from canEdit (item edit permission, which gates delete/reply).
	// A null/empty user_id has no provable author → admin-only, matching the
	// server's canEditComment.
	function canEditComment(c: Comment): boolean {
		return isAdmin || (!!c.user_id && c.user_id === currentUserId);
	}

	// "edited" when the body has been updated after creation. Reactions live in
	// separate tables and don't bump updated_at, so this is edit-specific.
	function isEdited(c: Comment): boolean {
		if (!c.updated_at || !c.created_at) return false;
		return new Date(c.updated_at).getTime() - new Date(c.created_at).getTime() > 1000;
	}

	async function saveComment(body: string) {
		await onEdit(comment.id, body); // throws → CommentEditor keeps the draft
		editing = false;
	}

	async function saveReply(replyId: string, body: string) {
		await onEdit(replyId, body);
		editingReplyId = null;
	}

	// Posts a reply via the host callback. Throws on failure so CommentEditor
	// keeps the draft; closes the form on success.
	async function submitReply(body: string) {
		submittingReply = true;
		try {
			await onReply(comment.id, body);
			showReplyForm = false;
		} catch (err) {
			throw err;
		} finally {
			submittingReply = false;
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
		{#if isEdited(comment)}
			<span class="edited-marker" title={`Edited ${new Date(comment.updated_at).toLocaleString()}`}>· edited</span>
		{/if}
		{#if canEditComment(comment) && !editing}
			<button class="edit-btn" type="button" onclick={() => { editing = true; }} title="Edit comment">
				<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
					<path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7" />
					<path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z" />
				</svg>
			</button>
		{/if}
		{#if canEdit}
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
		{/if}
	</div>

	{#if comment.activity_id}
		<div class="activity-label">commented on update</div>
	{/if}

	{#if editing}
		<div class="edit-compose">
			<CommentEditor
				{wsSlug}
				content={comment.body}
				placeholder="Edit comment…"
				submitLabel="Save"
				autofocus
				onSubmit={saveComment}
				onCancel={() => { editing = false; }}
			/>
		</div>
	{:else}
		<div class="comment-body prose">
			{@html renderMarkdown(comment.body, items, wsSlug, username, undefined, attachmentResolver, 'thumb-sm')}
		</div>
	{/if}

	<div class="comment-footer">
		<!--
			Existing reactions render with counts for everyone (read-only
			viewers see who reacted). The toggle / picker are gated on
			canEdit (PLAN-1100 / TASK-1107) — viewers cannot react.
		-->
		<div class="reactions-row">
				{#each reactionGroups as group (group.emoji)}
					<button
						class="reaction-chip"
						class:mine={hasMyReaction(comment.reactions, group.emoji)}
						type="button"
						title={group.actors.join(', ')}
						disabled={!canEdit}
						onclick={canEdit ? () => toggleReaction(group.emoji) : undefined}
					>
						<span class="reaction-emoji">{group.emoji}</span>
						<span class="reaction-count">{group.count}</span>
					</button>
				{/each}
				{#if canEdit}
					<ReactionPicker onSelect={handleAddReaction} />
				{/if}
		</div>
		{#if canEdit}
			<button class="reply-btn" type="button" onclick={() => { showReplyForm = !showReplyForm; }}>
				Reply
			</button>
		{/if}
	</div>

	{#if showReplyForm && canEdit}
		<div class="reply-compose">
			<CommentEditor
				{wsSlug}
				placeholder="Write a reply… (paste or drop an image to attach)"
				submitLabel="Reply"
				autofocus
				submitting={submittingReply}
				onSubmit={submitReply}
				onCancel={() => { showReplyForm = false; }}
			/>
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
						{#if isEdited(reply)}
							<span class="edited-marker" title={`Edited ${new Date(reply.updated_at).toLocaleString()}`}>· edited</span>
						{/if}
						{#if canEditComment(reply) && editingReplyId !== reply.id}
							<button class="edit-btn" type="button" onclick={() => { editingReplyId = reply.id; }} title="Edit reply">
								<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
									<path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7" />
									<path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z" />
								</svg>
							</button>
						{/if}
						{#if canEdit}
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
						{/if}
					</div>

					{#if editingReplyId === reply.id}
						<div class="edit-compose">
							<CommentEditor
								{wsSlug}
								content={reply.body}
								placeholder="Edit reply…"
								submitLabel="Save"
								autofocus
								onSubmit={(body) => saveReply(reply.id, body)}
								onCancel={() => { editingReplyId = null; }}
							/>
						</div>
					{:else}
						<div class="reply-body prose">
							{@html renderMarkdown(reply.body, items, wsSlug, username, undefined, attachmentResolver, 'thumb-sm')}
						</div>
					{/if}

						<div class="reactions-row">
							{#each replyReactionGroups as group (group.emoji)}
								<button
									class="reaction-chip"
									class:mine={hasMyReaction(reply.reactions, group.emoji)}
									type="button"
									title={group.actors.join(', ')}
									disabled={!canEdit}
									onclick={canEdit ? () => toggleReplyReaction(reply, group.emoji) : undefined}
								>
									<span class="reaction-emoji">{group.emoji}</span>
									<span class="reaction-count">{group.count}</span>
								</button>
							{/each}
							{#if canEdit}
								<ReactionPicker onSelect={(emoji) => handleAddReplyReaction(reply, emoji)} />
							{/if}
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

	/* Edit button mirrors the delete button (hover-revealed) but uses the
	   accent-blue affordance instead of the destructive red. */
	.edit-btn {
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
	.reply-card:hover .reply-header > .delete-btn,
	.comment-card:hover .comment-header > .edit-btn,
	.reply-card:hover .reply-header > .edit-btn {
		opacity: 1;
	}

	.delete-btn:hover {
		color: var(--accent-red);
		background: color-mix(in srgb, var(--accent-red) 10%, transparent);
	}

	.edit-btn:hover {
		color: var(--accent-blue);
		background: color-mix(in srgb, var(--accent-blue) 10%, transparent);
	}

	.edited-marker {
		font-size: 0.7em;
		color: var(--text-muted);
		font-style: italic;
	}

	.edit-compose {
		margin: var(--space-2) 0;
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
		/* `.prose` sets max-width: var(--content-max-width) for long-form item
		   bodies; comments live inside an already-constrained timeline column
		   and should fill it instead of shrinking to 960px. */
		max-width: none;
		/* `.prose` also pins font-family to var(--font-content). Comments should
		   keep using the surrounding UI font (currently identical, but make the
		   relationship explicit so a future divergence doesn't silently change
		   comment typography). */
		font-family: inherit;
		/* `.prose table { width: 100% }` plus padded cells can produce wider-
		   than-column tables inside the indented replies; allow the rendered
		   markdown subtree to scroll horizontally rather than spill out. */
		overflow-x: auto;
	}

	.comment-body :global(p:last-child),
	.reply-body :global(p:last-child) {
		margin-bottom: 0;
	}

	.comment-body :global(p:first-child),
	.reply-body :global(p:first-child) {
		margin-top: 0;
	}

	/* Inline attachment images render as compact thumbnails (IDEA-1660).
	   The `.prose img { max-width: 100% }` base would let a pasted screenshot
	   fill the comment column; cap it to a small box and signal it's
	   clickable. Click-to-expand is handled by the timeline's delegated
	   handler, which opens the full-resolution image in a lightbox. */
	.comment-body :global(img[data-attachment-id]),
	.reply-body :global(img[data-attachment-id]) {
		max-width: 280px;
		max-height: 180px;
		width: auto;
		height: auto;
		object-fit: contain;
		border: 1px solid var(--border);
		background: var(--bg-secondary);
		cursor: zoom-in;
		vertical-align: middle;
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
		margin-top: var(--space-2);
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
