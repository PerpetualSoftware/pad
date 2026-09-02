<script lang="ts">
	/**
	 * The timeline's rendered entry list — rail chrome plus one card per entry
	 * (IDEA-2843). Extracted from `ItemTimeline` unchanged, so that the SAME
	 * list can render in two places: comments under the item content, and
	 * changes/versions in the pane's Activity and Versions tabs.
	 *
	 * It is PRESENTATION ONLY. Fetching, the SSE subscription, pagination, the
	 * attachment-metadata probe, the paint fence and every mutation stay in
	 * `ItemTimeline`, which remains the single owner — the constraint on this
	 * work is one subscription and one composer, and the way to keep that true
	 * is for this component to have no data of its own.
	 *
	 * The comment-card callbacks are threaded rather than dispatched because
	 * their handlers live with the data they mutate. A view that renders no
	 * comments (the changes list) simply omits them.
	 */
	import type { TimelineEntry, Item } from '$lib/types';
	import type { AttachmentMeta } from '$lib/markdown/attachments';
	import TimelineCommentCard from './TimelineCommentCard.svelte';
	import TimelineActivityCard from './TimelineActivityCard.svelte';
	import TimelineVersionCard from './TimelineVersionCard.svelte';
	import TimelineStructuredCard from './TimelineStructuredCard.svelte';

	interface Props {
		/** The entries to render — already kind-filtered by the owner. */
		entries: TimelineEntry[];
		/**
		 * The list container. Bindable because the owner attaches the
		 * delegated lightbox listeners and the imperative image-a11y pass to
		 * it; both are about inline images in comment BODIES, so they stay
		 * with the owner rather than moving here.
		 */
		listEl?: HTMLElement;
		/**
		 * Render the "no entries yet" line. Computed by the owner, and
		 * deliberately NOT `entries.length === 0` here: the owner's condition
		 * is over the WHOLE feed, so a tab that filters everything out renders
		 * an empty list rather than claiming the item has no history.
		 */
		showEmpty?: boolean;

		wsSlug: string;
		username?: string;
		items?: Item[];
		hostToken?: string;

		// Comment cards.
		currentUserId?: string;
		canEdit?: boolean;
		frozen?: boolean;
		isAdmin?: boolean;
		attachmentResolver?: (uuid: string) => AttachmentMeta | null;
		/**
		 * Optional here, required by `TimelineCommentCard`. A list rendering no
		 * comments — the changes/versions view — has nothing to hand it, so the
		 * defaults below stand in and are never reached: a card that could call
		 * one is a comment card, and a comment card only renders when the owner
		 * supplied the real handlers.
		 */
		onDelete?: (commentId: string) => void;
		onReply?: (commentId: string, body: string) => void | Promise<void>;
		onEdit?: (commentId: string, body: string) => void | Promise<void>;
		onReaction?: (commentId: string, emoji: string) => void;
		onRemoveReaction?: (commentId: string, emoji: string) => void;

		// Version cards.
		itemSlug?: string;
		currentContent?: string;
		onRestore?: (item: Item) => void;
		flushBeforeRestore?: () => Promise<void>;
		restoreFrozen?: boolean;
	}

	let {
		entries,
		listEl = $bindable(),
		showEmpty = false,
		wsSlug,
		username = '',
		items = [],
		hostToken = '',
		currentUserId = '',
		canEdit = false,
		frozen = false,
		isAdmin = false,
		attachmentResolver,
		onDelete = () => {},
		onReply = () => {},
		onEdit = () => {},
		onReaction = () => {},
		onRemoveReaction = () => {},
		itemSlug = '',
		currentContent = '',
		onRestore,
		flushBeforeRestore,
		restoreFrozen = false
	}: Props = $props();

	function dotClass(kind: TimelineEntry['kind']): string {
		if (kind === 'comment') return 'dot-comment';
		if (kind === 'version') return 'dot-version';
		if (kind === 'note') return 'dot-note';
		if (kind === 'decision') return 'dot-decision';
		return 'dot-activity';
	}
</script>

<div class="entry-list" bind:this={listEl}>
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
						{username}
						{items}
						{currentUserId}
						{canEdit}
						{frozen}
						{isAdmin}
						{hostToken}
						{attachmentResolver}
						{onDelete}
						{onReply}
						{onEdit}
						{onReaction}
						{onRemoveReaction}
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
						{flushBeforeRestore}
						frozen={frozen || restoreFrozen}
					/>
				<!-- No `&& entry.note` guard, unlike the kinds above: the card is
				     null-safe, and a payload-less entry still occupies a rail. Requiring
				     the payload turns a partial entry into a blank rail with no card at
				     all, which reads as a rendering fault rather than as a thin entry. -->
				{:else if entry.kind === 'note'}
					<TimelineStructuredCard
						kind="note"
						note={entry.note}
						actor={entry.actor}
						createdAt={entry.created_at}
					/>
				{:else if entry.kind === 'decision'}
					<TimelineStructuredCard
						kind="decision"
						decision={entry.decision}
						actor={entry.actor}
						createdAt={entry.created_at}
					/>
				{/if}
			</div>
		</div>
	{/each}

	{#if showEmpty}
		<div class="empty">No timeline entries yet.</div>
	{/if}
</div>

<style>
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

	.dot-note {
		background: var(--accent-cyan);
	}

	.dot-decision {
		background: var(--accent-orange);
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
</style>
