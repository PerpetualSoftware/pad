<script lang="ts">
	import { page } from '$app/state';
	import { api, isNotFoundError } from '$lib/api/client';
	import type { ChatConversationSummary } from '$lib/types';

	// A placeholder: the thread itself arrives in a later change. It reads the
	// conversation to name the agent and never marks anything read.
	let raw = $derived(page.params.n ?? '');
	let n = $derived(/^[1-9][0-9]*$/.test(raw) ? Number(raw) : null);

	let conversation = $state<ChatConversationSummary | null>(null);
	let notFound = $state(false);
	let failed = $state(false);
	let seq = 0;

	$effect(() => {
		const target = n;
		const mine = ++seq;
		conversation = null;
		failed = false;
		notFound = target === null;
		if (target === null) return;
		api.chat
			.getConversation(target)
			.then((c) => {
				if (mine === seq) conversation = c;
			})
			.catch((err: unknown) => {
				if (mine !== seq) return;
				if (isNotFoundError(err)) notFound = true;
				else failed = true;
			});
	});
</script>

<svelte:head>
	<title>Chat · Pad</title>
</svelte:head>

<header class="chat-header">
	<a class="all-chats" href="/messages">All conversations</a>
</header>

{#if notFound}
	<h1>Conversation not found</h1>
{:else if failed}
	<h1>Could not load this conversation</h1>
{:else}
	<h1>{conversation ? conversation.title || conversation.agent_key : 'Chat'}</h1>
	<p class="notice">
		Messages open here in the next update. Use <code>pad chat show C{n}</code> in the meantime.
	</p>
{/if}

<style>
	.chat-header {
		display: flex;
		gap: var(--space-4);
		margin-bottom: var(--space-6);
	}
	.all-chats {
		color: var(--text-secondary);
		font-size: 0.85rem;
	}
	h1 {
		margin: 0 0 var(--space-3);
		font-size: 1.5rem;
	}
	.notice {
		color: var(--text-secondary);
	}
</style>
