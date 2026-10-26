<script lang="ts">
	import { onDestroy, onMount } from 'svelte';
	import { page } from '$app/state';
	import ChatBackLink from '$lib/components/chat/ChatBackLink.svelte';
	import { api, isNotFoundError } from '$lib/api/client';
	import PresenceDot from '$lib/components/chat/PresenceDot.svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { chatInbox } from '$lib/stores/chatInbox.svelte';
	import type { ChatConversationSummary } from '$lib/types';

	// A placeholder: the thread itself arrives in a later change. It reads the
	// conversation to name the agent and never marks anything read.
	let raw = $derived(page.params.n ?? '');
	let n = $derived(/^[1-9][0-9]*$/.test(raw) ? Number(raw) : null);

	let conversation = $state<ChatConversationSummary | null>(null);
	let notFound = $state(false);
	let failed = $state(false);
	let seq = 0;
	// Bumped by "Try again" so the load effect below re-runs (TASK-4044 2b-i, ruling 90709f40).
	let retry = $state(0);

	// The presence dot polls only while this page is open.
	onMount(() => chatInbox.startPresence());
	onDestroy(() => chatInbox.stopPresence());

	$effect(() => {
		const target = n;
		void retry;
		const mine = ++seq;
		conversation = null;
		failed = false;
		notFound = target === null;
		if (target === null) return;
		const isSameIdentity = authStore.identityFence();
		api.chat
			.getConversation(target)
			.then((c) => {
				if (mine === seq && isSameIdentity()) conversation = c;
			})
			.catch((err: unknown) => {
				if (mine !== seq || !isSameIdentity()) return;
				if (isNotFoundError(err)) notFound = true;
				else failed = true;
			});
	});
</script>

<svelte:head>
	<title>Chat · Pad</title>
</svelte:head>

<header class="chat-header">
	<ChatBackLink />
	<a class="all-chats" href="/messages">All conversations</a>
	{#if conversation}
		<span class="presence">
			<PresenceDot
				state={chatInbox.presenceFor(conversation.agent_key)}
				checkedAt={chatInbox.checkedAt}
			/>
			{conversation.agent_key}
		</span>
	{/if}
</header>

{#if notFound}
	<h1>Conversation not found</h1>
{:else if failed}
	<h1>Could not load this conversation</h1>
	<button type="button" class="retry" onclick={() => retry++}>Try again</button>
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
	.presence {
		display: inline-flex;
		align-items: center;
		gap: var(--space-2);
		margin-left: auto;
		font-size: 0.85rem;
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
