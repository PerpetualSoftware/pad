<script lang="ts">
	import type { Snippet } from 'svelte';
	import UnreadBadge from '$lib/components/chat/UnreadBadge.svelte';
	import { chatStore } from '$lib/stores/chat.svelte';

	// The one entry to /messages (TASK-4044 PR 2a-ii). Three placements share it;
	// each passes the class of its siblings. A parent's scoped styles do not reach
	// into this element, so the parent styles it with :global(.<class>), and the
	// lead/trail snippets carry the parent's own scope for its icon and chevron.
	let {
		class: className = '',
		role,
		onclick,
		lead,
		trail
	}: {
		class?: string;
		role?: 'menuitem';
		onclick?: () => void;
		lead?: Snippet;
		trail?: Snippet;
	} = $props();

	let count = $derived(chatStore.unreadTotal);
</script>

<a
	href="/messages"
	class={className}
	{role}
	{onclick}
	aria-label={count > 0 ? `Chat, ${count} unread` : 'Chat'}
>
	{@render lead?.()}
	<span class="chat-label">Chat</span>
	<UnreadBadge {count} />
	{@render trail?.()}
</a>

<style>
	.chat-label {
		flex: 1;
		min-width: 0;
	}
</style>
