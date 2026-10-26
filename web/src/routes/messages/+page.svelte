<script lang="ts">
	import { onMount } from 'svelte';
	import ChatBackLink from '$lib/components/chat/ChatBackLink.svelte';
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
	<ChatBackLink />
	<h1>Chat</h1>
</header>

<!-- The list reloads only when chatStore's unread fetch succeeds, so a failed
     list fetch beside a good unread fetch has no other way back (TASK-4044 r1):
     chatStore's retry re-arms only for a failed UNREAD fetch.
     A failed reload keeps the last list, and says so (r2). -->
{#if chatInbox.failed}
	<p class="status">
		{chatInbox.loaded ? "Couldn't refresh your conversations." : 'Could not load your conversations.'}
	</p>
	<button type="button" onclick={() => void chatInbox.load()}>Try again</button>
{/if}
{#if chatInbox.loaded}
	<ConversationList conversations={chatInbox.conversations} />
{:else if !chatInbox.failed}
	<p class="status">Loading...</p>
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
