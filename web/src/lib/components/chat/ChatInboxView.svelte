<script lang="ts">
	import { onMount } from 'svelte';
	import Button from '$lib/components/common/Button.svelte';
	import ContentSkeleton from '$lib/components/common/ContentSkeleton.svelte';
	import ChatBackLink from '$lib/components/chat/ChatBackLink.svelte';
	import ChatHeader from '$lib/components/chat/ChatHeader.svelte';
	import RosterRow from '$lib/components/chat/RosterRow.svelte';
	import ConversationList from '$lib/components/chat/ConversationList.svelte';
	import ChatEmptyState from '$lib/components/chat/ChatEmptyState.svelte';
	import { chatEmptyKind } from '$lib/chat/emptyState';
	import { chatInbox } from '$lib/stores/chatInbox.svelte';
	import { chatSurface } from '$lib/stores/chatSurface.svelte';

	// `variant` says where it renders: the /messages route, or the Chat host (dock
	// or drawer) in the shell, which has its own Close.
	let { variant = 'route' }: { variant?: 'route' | 'host' } = $props();

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

	// The help replaces the bare "No conversations yet." only when both reads answered.
	const emptyKind = $derived(
		chatEmptyKind({
			loaded: chatInbox.loaded,
			failed: chatInbox.failed,
			conversations: chatInbox.conversations,
			sessions: chatInbox.sessions,
			rosterLength: chatInbox.roster.length
		})
	);

	// Agents with a conversation are cards in the list below, with their presence.
	// The roster is who you could START a chat with (BUG-4321, product 11-05).
	const startable = $derived(chatInbox.roster.filter((e) => e.number === null));

	// Open the chat only once the POST has answered: a null means nothing started.
	async function ensure(key: string): Promise<void> {
		const n = await chatInbox.ensureDM(key);
		if (n !== null && mounted) await chatSurface.open(`/messages/${n}`);
	}
</script>

{#snippet backLink()}<ChatBackLink />{/snippet}

<!-- TASK-4515: the shared header row; the route puts its Back link where the thread's chevron goes. -->
<ChatHeader
	title="Chat"
	onClose={variant === 'host' ? () => chatSurface.close() : undefined}
	lead={variant === 'route' ? backLink : undefined}
/>

<div class="pane">
	<!-- The list reloads only when chatStore's unread fetch succeeds, so a failed
	     list fetch beside a good unread fetch has no other way back (TASK-4044):
	     chatStore's retry re-arms only for a failed UNREAD fetch.
	     A failed reload keeps the last list, and says so. -->
	{#if chatInbox.failed}
		<div class="notice">
			<span>{chatInbox.loaded ? "Couldn't refresh your conversations." : 'Could not load your conversations.'}</span>
			<Button size="sm" touch onclick={() => void chatInbox.load()}>Try again</Button>
		</div>
	{/if}

	<ChatEmptyState />

	{#if startable.length > 0}
		<!-- With no conversation yet, ChatEmptyState already says "Start a conversation". -->
		{#if emptyKind !== 'start'}<h2 class="section-label">Start a conversation</h2>{/if}
		<ul class="roster" aria-label="Agents to start a chat with">
			{#each startable as entry (entry.key)}
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

	{#if chatInbox.loaded && emptyKind !== 'no-agents'}
		{#if startable.length > 0}<h2 class="section-label">Conversations</h2>{/if}
		<ConversationList conversations={chatInbox.conversations} />
	{:else if !chatInbox.loaded && !chatInbox.failed}
		<ContentSkeleton variant="inline" />
	{/if}
</div>

<style>
	.pane {
		flex: 1;
		min-height: 0;
		display: flex;
		flex-direction: column;
		padding: var(--space-4);
	}
	/* The sidebar's section-label style ("COLLECTIONS"). */
	.section-label {
		margin: 0 0 var(--space-2) var(--space-1);
		font-size: 11px;
		font-weight: 600;
		letter-spacing: 0.06em;
		text-transform: uppercase;
		color: var(--text-muted);
	}
	.roster {
		list-style: none;
		margin: 0 0 var(--space-5);
		padding: 0;
		border: 1px solid var(--card-border);
		border-radius: var(--radius-lg);
		background: var(--card-bg);
		overflow: hidden;
	}
	.roster li + li {
		border-top: 1px solid var(--border-subtle);
	}
	.notice {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		margin-bottom: var(--space-4);
		padding: var(--space-2) var(--space-3);
		border: 1px solid color-mix(in srgb, var(--accent-red) 40%, transparent);
		border-radius: var(--radius);
		background: color-mix(in srgb, var(--accent-red) 10%, transparent);
		font-size: 13px;
		color: var(--text-primary);
	}
	.notice span {
		flex: 1;
	}
</style>
