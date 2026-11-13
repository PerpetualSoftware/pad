<script lang="ts">
	import ChatAvatar from '$lib/components/chat/ChatAvatar.svelte';
	import PresenceDot from '$lib/components/chat/PresenceDot.svelte';
	import UnreadBadge from '$lib/components/chat/UnreadBadge.svelte';
	import { listTime } from '$lib/chat/grouping';
	import { chatStore } from '$lib/stores/chat.svelte';
	import { chatInbox } from '$lib/stores/chatInbox.svelte';
	import { chatSurface } from '$lib/stores/chatSurface.svelte';
	import type { ChatConversationSummary } from '$lib/types';

	let { conversations }: { conversations: ChatConversationSummary[] } = $props();

	function label(c: ChatConversationSummary): string {
		return c.title || c.agent_key || `Conversation ${c.number}`;
	}

	// "2 armed sessions" only when it is more than one: one is the usual case.
	function armedDetail(c: ChatConversationSummary): string {
		const n = chatInbox.armedCount(c.agent_key);
		return n > 1 ? `${n} armed sessions` : '';
	}

	function preview(c: ChatConversationSummary): string {
		const m = c.last_message;
		if (!m) return 'No messages yet';
		if (m.deleted) return 'Message deleted';
		return m.excerpt;
	}
</script>

{#if conversations.length === 0}
	<p class="empty">No conversations yet.</p>
{:else}
	<!-- TASK-4515: two-line rows; the name (and presence) over a full-width preview, the time over the badge. -->
	<ul class="conversation-list">
		{#each conversations as c (c.id)}
			{@const unread = chatStore.unreadByNumber[c.number] ?? 0}
			{@const when = c.last_message ? listTime(c.last_message.created_at) : ''}
			<li>
				<a
					class="row"
					class:unread={unread > 0}
					href="/messages/{c.number}"
					onclick={(e) => chatSurface.handleLinkClick(e, `/messages/${c.number}`)}
				>
					<ChatAvatar key={c.kind === 'direct' ? c.agent_key : label(c)} size="lg" />
					<span class="main">
						<span class="line">
							<span class="name" title={label(c)}>{label(c)}</span>
							<!-- BUG-4321: the card carries the agent's presence, so the agent needs no second row. A group's key is not an agent. -->
							{#if c.kind === 'direct'}
								<PresenceDot
									state={chatInbox.presenceFor(c.agent_key)}
									checkedAt={chatInbox.checkedAt}
									detail={armedDetail(c)}
									showLabel
								/>
							{/if}
						</span>
						<span class="preview" class:deleted={c.last_message?.deleted}>{preview(c)}</span>
					</span>
					<span class="side">
						{#if when}<time datetime={c.last_message!.created_at}>{when}</time>{/if}
						<UnreadBadge count={unread} />
					</span>
				</a>
			</li>
		{/each}
	</ul>
{/if}

<style>
	.conversation-list {
		list-style: none;
		margin: 0;
		padding: 0;
		border: 1px solid var(--card-border);
		border-radius: var(--radius-lg);
		background: var(--card-bg);
		overflow: hidden;
	}
	li + li {
		border-top: 1px solid var(--border-subtle);
	}
	.row {
		display: grid;
		grid-template-columns: 36px minmax(0, 1fr) auto;
		column-gap: var(--space-3);
		align-items: center;
		box-sizing: border-box;
		min-height: 60px;
		padding: var(--space-2) var(--space-3);
		color: var(--text-primary);
		text-decoration: none;
	}
	.row:hover {
		background: var(--bg-hover);
		text-decoration: none;
	}
	.row:focus-visible {
		outline: 2px solid color-mix(in srgb, var(--accent-primary) 65%, transparent);
		outline-offset: -2px;
	}
	.main {
		display: flex;
		flex-direction: column;
		gap: 2px;
		min-width: 0;
	}
	.line {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		min-width: 0;
	}
	.name {
		min-width: 0;
		font-weight: 600;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.line :global(.presence-wrap) {
		flex-shrink: 0;
		font-size: 12px;
		color: var(--text-secondary);
		white-space: nowrap;
	}
	.preview {
		color: var(--text-secondary);
		font-size: 13px;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.row.unread .preview {
		color: var(--text-primary);
	}
	.preview.deleted {
		font-style: italic;
		color: var(--text-muted);
	}
	.side {
		display: flex;
		flex-direction: column;
		align-items: flex-end;
		gap: 4px;
		font-size: 12px;
		color: var(--text-muted);
		white-space: nowrap;
	}
	.empty {
		margin: 0;
		color: var(--text-muted);
	}
</style>
