<script lang="ts">
	import { goto } from '$app/navigation';
	import { onMount } from 'svelte';
	import ChatBackLink from '$lib/components/chat/ChatBackLink.svelte';
	import RosterRow from '$lib/components/chat/RosterRow.svelte';
	import ConversationList from '$lib/components/chat/ConversationList.svelte';
	import { chatInbox } from '$lib/stores/chatInbox.svelte';

	// A late POST answer must not pull the user back after they leave (TASK-4044).
	let mounted = true;

	// The roster's dots poll sessions only while this page is open.
	onMount(() => {
		chatInbox.start();
		chatInbox.startPresence();
		return () => {
			mounted = false;
			chatInbox.stop();
			chatInbox.stopPresence();
		};
	});

	// Open the chat only once the POST has answered: a null means nothing started.
	async function ensure(key: string): Promise<void> {
		const n = await chatInbox.ensureDM(key);
		if (n !== null && mounted) await goto(`/messages/${n}`);
	}
</script>

<svelte:head>
	<title>Chat · Pad</title>
</svelte:head>

<header class="chat-header">
	<ChatBackLink />
	<h1>Chat</h1>
</header>

{#if chatInbox.roster.length > 0}
	<ul class="roster" aria-label="Agents">
		{#each chatInbox.roster as entry (entry.key)}
			<li>
				<RosterRow
					{entry}
					checkedAt={chatInbox.checkedAt}
					pending={chatInbox.ensuring.has(entry.key)}
					rateLimited={!!chatInbox.rateLimited[entry.key]}
					failed={!!chatInbox.ensureFailed[entry.key]}
					onensure={ensure}
				/>
			</li>
		{/each}
	</ul>
{/if}

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
	.roster {
		list-style: none;
		margin: 0 0 var(--space-6);
		padding: 0;
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		overflow: hidden;
	}
	.roster li + li {
		border-top: 1px solid var(--border);
	}
	.status {
		color: var(--text-muted);
	}
</style>
