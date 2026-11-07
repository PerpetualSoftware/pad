<script lang="ts">
	import UnreadBadge from '$lib/components/chat/UnreadBadge.svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { chatSetting } from '$lib/stores/chatSetting.svelte';
	import { chatStore } from '$lib/stores/chat.svelte';
	import { chatSurface } from '$lib/stores/chatSurface.svelte';

	// The visible Chat control in the top bar (TASK-4315 PR 3b). A plain click
	// opens the Chat surface over the current page; a modified click is the
	// direct entry. `compact` is the icon-only form beside the collapsed bar's
	// expand control. Nothing renders while Chat is off or nobody is signed in.
	let { compact = false }: { compact?: boolean } = $props();

	let count = $derived(chatStore.unreadTotal);
</script>

{#if authStore.authenticated && chatSetting.enabled}
	<a
		href="/messages"
		class="chat-button"
		class:compact
		aria-label={count > 0 ? `Chat, ${count} unread` : 'Chat'}
		aria-current={chatSurface.isOpen ? 'true' : undefined}
		onclick={(e) => chatSurface.handleLinkClick(e, '/messages')}
	>
		<svg width="14" height="14" viewBox="0 0 16 16" fill="none" aria-hidden="true">
			<path
				d="M2.5 3.5h11v7h-5l-3 2.5v-2.5h-3z"
				stroke="currentColor"
				stroke-width="1.4"
				stroke-linejoin="round"
			/>
		</svg>
		{#if !compact}<span class="chat-label">Chat</span>{/if}
		<UnreadBadge {count} />
	</a>
{/if}

<style>
	.chat-button {
		display: inline-flex;
		align-items: center;
		gap: var(--space-1);
		height: 28px;
		padding: 0 var(--space-2);
		border-radius: var(--radius-sm);
		color: var(--text-secondary);
		font-size: 0.85rem;
		text-decoration: none;
	}
	.chat-button:hover,
	.chat-button[aria-current='true'] {
		color: var(--text-primary);
		background: var(--bg-hover);
		text-decoration: none;
	}
	.chat-button.compact {
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-top: none;
		border-radius: 0 0 var(--radius) var(--radius);
		height: 20px;
	}
</style>
