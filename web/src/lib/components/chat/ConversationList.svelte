<script lang="ts">
	import UnreadBadge from '$lib/components/chat/UnreadBadge.svelte';
	import { chatStore } from '$lib/stores/chat.svelte';
	import type { ChatConversationSummary } from '$lib/types';

	let { conversations }: { conversations: ChatConversationSummary[] } = $props();

	function label(c: ChatConversationSummary): string {
		return c.title || c.agent_key || `Conversation ${c.number}`;
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
					<span class="name">{label(c)}</span>
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
