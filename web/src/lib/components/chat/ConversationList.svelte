<script lang="ts">
	import PresenceDot from '$lib/components/chat/PresenceDot.svelte';
	import UnreadBadge from '$lib/components/chat/UnreadBadge.svelte';
	import { chatStore } from '$lib/stores/chat.svelte';
	import { chatInbox } from '$lib/stores/chatInbox.svelte';
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
	<ul class="conversation-list">
		{#each conversations as c (c.id)}
			<li>
				<a class="row" href="/messages/{c.number}">
					<span class="who">
						<span class="name">{label(c)}</span>
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
					<UnreadBadge count={chatStore.unreadByNumber[c.number] ?? 0} />
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
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		overflow: hidden;
	}
	li + li {
		border-top: 1px solid var(--border);
	}
	.row {
		display: grid;
		grid-template-columns: minmax(6rem, 12rem) 1fr auto;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-3) var(--space-4);
		color: var(--text-primary);
		text-decoration: none;
	}
	.row:hover {
		background: var(--bg-hover);
		text-decoration: none;
	}
	.who {
		display: flex;
		flex-direction: column;
		gap: 2px;
		min-width: 0;
	}
	.name {
		font-weight: 600;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.preview {
		color: var(--text-secondary);
		font-size: 0.9rem;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.preview.deleted {
		font-style: italic;
		color: var(--text-muted);
	}
	.empty {
		color: var(--text-muted);
	}
</style>
