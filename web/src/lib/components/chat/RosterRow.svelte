<script lang="ts">
	import PresenceDot from '$lib/components/chat/PresenceDot.svelte';
	import UnreadBadge from '$lib/components/chat/UnreadBadge.svelte';
	import { chatStore } from '$lib/stores/chat.svelte';
	import type { RosterEntry } from '$lib/stores/chatInbox.svelte';

	let {
		entry,
		checkedAt = null,
		pending = false,
		rateLimited = false,
		failed = false,
		onensure
	}: {
		entry: RosterEntry;
		checkedAt?: number | null;
		pending?: boolean;
		rateLimited?: boolean;
		failed?: boolean;
		onensure: (key: string) => void;
	} = $props();
</script>

{#if entry.number !== null}
	<a class="row" href="/messages/{entry.number}">
		<PresenceDot state={entry.presence} {checkedAt} />
		<span class="name">{entry.key}</span>
		<UnreadBadge count={chatStore.unreadByNumber[entry.number] ?? 0} />
	</a>
{:else}
	<button
		type="button"
		class="row"
		disabled={pending || rateLimited}
		onclick={() => onensure(entry.key)}
	>
		<PresenceDot state={entry.presence} {checkedAt} />
		<span class="name">{entry.key}</span>
		<span class="action">Start chat</span>
	</button>
	{#if rateLimited}
		<p class="note" role="status">Too many new chats right now. Try again in a few seconds.</p>
	{:else if failed}
		<p class="note" role="status">Couldn't start the chat. Try again.</p>
	{/if}
{/if}

<style>
	.row {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		width: 100%;
		padding: var(--space-3) var(--space-4);
		border: 0;
		background: none;
		font: inherit;
		color: var(--text-primary);
		text-align: left;
		text-decoration: none;
		cursor: pointer;
	}
	.row:hover:not(:disabled) {
		background: var(--bg-hover);
		text-decoration: none;
	}
	.row:disabled {
		cursor: default;
		opacity: 0.6;
	}
	.name {
		flex: 1;
		font-weight: 600;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.action {
		color: var(--text-secondary);
		font-size: 0.9rem;
	}
	.note {
		margin: 0;
		padding: 0 var(--space-4) var(--space-3);
		color: var(--text-muted);
		font-size: 0.9rem;
	}
</style>
