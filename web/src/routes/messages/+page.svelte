<script lang="ts">
	import { onMount } from 'svelte';
	import ConversationList from '$lib/components/chat/ConversationList.svelte';
	import { chatInbox } from '$lib/stores/chatInbox.svelte';

	onMount(() => {
		chatInbox.start();
		return () => chatInbox.stop();
	});
</script>

<svelte:head>
	<title>Chat · Pad</title>
</svelte:head>

<header class="chat-header">
	<h1>Chat</h1>
</header>

{#if !chatInbox.loaded && chatInbox.failed}
	<p class="status">Could not load your conversations.</p>
	<!-- The list reloads only when chatStore's unread fetch succeeds, so a failed
	     list fetch beside a good unread fetch has no other way back (TASK-4044 r1). -->
	<button type="button" onclick={() => void chatInbox.load()}>Try again</button>
{:else if !chatInbox.loaded}
	<p class="status">Loading...</p>
{:else}
	<ConversationList conversations={chatInbox.conversations} />
{/if}

<style>
	.chat-header {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		margin-bottom: var(--space-6);
	}
	h1 {
		margin: 0;
		font-size: 1.5rem;
	}
	.status {
		color: var(--text-muted);
	}
</style>
