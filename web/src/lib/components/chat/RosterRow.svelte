<script lang="ts">
	import ChatAvatar from '$lib/components/chat/ChatAvatar.svelte';
	import PresenceDot from '$lib/components/chat/PresenceDot.svelte';
	import UnreadBadge from '$lib/components/chat/UnreadBadge.svelte';
	import { chatStore } from '$lib/stores/chat.svelte';
	import { chatSurface } from '$lib/stores/chatSurface.svelte';
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

<!-- TASK-4515: avatar, the name over its presence, and a "Start chat" pill. The whole row is
     the control (the one raw button the census allows here); the pill is drawn, not a button. -->
{#if entry.number !== null}
	<a
		class="row"
		href="/messages/{entry.number}"
		onclick={(e) => chatSurface.handleLinkClick(e, `/messages/${entry.number}`)}
	>
		<ChatAvatar key={entry.key} size="lg" />
		<span class="main">
			<span class="name" title={entry.key}>{entry.key}</span>
			<PresenceDot state={entry.presence} {checkedAt} showLabel />
		</span>
		<UnreadBadge count={chatStore.unreadByNumber[entry.number] ?? 0} />
	</a>
{:else}
	<button
		type="button"
		class="row"
		disabled={pending || rateLimited}
		onclick={() => onensure(entry.key)}
	>
		<ChatAvatar key={entry.key} size="lg" />
		<span class="main">
			<span class="name" title={entry.key}>{entry.key}</span>
			<PresenceDot state={entry.presence} {checkedAt} showLabel />
		</span>
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
		display: grid;
		grid-template-columns: 36px minmax(0, 1fr) auto;
		column-gap: var(--space-3);
		align-items: center;
		box-sizing: border-box;
		width: 100%;
		min-height: 60px;
		margin: 0;
		padding: var(--space-2) var(--space-3);
		border: 0;
		border-radius: 0;
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
	.row:focus-visible {
		outline: 2px solid color-mix(in srgb, var(--accent-primary) 65%, transparent);
		outline-offset: -2px;
	}
	.row:disabled {
		cursor: default;
		opacity: 0.6;
	}
	.main {
		display: flex;
		flex-direction: column;
		gap: 2px;
		min-width: 0;
	}
	.name {
		font-weight: 600;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.main :global(.presence-wrap) {
		font-size: 12px;
		color: var(--text-secondary);
	}
	/* Drawn as Button secondary sm. */
	.action {
		padding: 3px 10px;
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--bg-tertiary);
		font-size: 12px;
		font-weight: 600;
		line-height: 1.5;
		white-space: nowrap;
		color: var(--text-primary);
	}
	.row:hover:not(:disabled) .action {
		border-color: var(--border-strong);
	}
	.note {
		margin: 0;
		padding: 0 var(--space-3) var(--space-2) calc(var(--space-3) + 36px + var(--space-3));
		color: var(--text-muted);
		font-size: 12px;
	}
</style>
