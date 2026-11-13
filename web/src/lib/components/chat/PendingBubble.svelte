<script lang="ts">
	import Button from '$lib/components/common/Button.svelte';
	import type { PendingSend } from '$lib/chat/model';

	// A send the server has not confirmed (TASK-4044 PR 3b): shown at once as
	// "Sending…", and as "Failed to send" with Retry (the SAME op_key) and Discard
	// when it did not go through. It has no message menu: it is not a message yet.
	// TASK-4515: drawn as your own bubble, dashed; `head` as for a confirmed row.
	let {
		pending,
		head = true,
		onRetry,
		onDiscard
	}: {
		pending: PendingSend;
		head?: boolean;
		onRetry: (opKey: string) => void;
		onDiscard: (opKey: string) => void;
	} = $props();
</script>

<article class="bubble mine pending" class:head class:failed={pending.status === 'failed'} data-op-key={pending.op_key}>
	{#if head}
		<header class="meta"><span class="author">You</span><span class="sr-only">Human</span></header>
	{:else}
		<span class="sr-only"><span>You</span> <span>Human</span></span>
	{/if}
	<p class="body">{pending.body}</p>
	<div class="state-row">
		<!-- Mounted for the bubble's life, only its text changes (r5 H5). -->
		<p class="state" role="status">{pending.status === 'sending' ? 'Sending…' : pending.status === 'unconfirmed' ? 'Send not confirmed' : 'Failed to send.'}</p>
		{#if pending.status !== 'sending'}
			<Button size="sm" touch onclick={() => onRetry(pending.op_key)}>Retry</Button>
			<Button variant="ghost" size="sm" touch onclick={() => onDiscard(pending.op_key)}>Discard</Button>
		{/if}
	</div>
</article>

<style>
	.bubble {
		box-sizing: border-box;
		min-width: 0;
		align-self: stretch;
		display: flex;
		flex-direction: column;
		align-items: flex-end;
		margin: 2px calc(-1 * var(--space-2)) 0;
		padding: 2px var(--space-2);
	}
	.bubble.head { margin-top: var(--space-3); }
	.meta {
		display: flex;
		align-items: center;
		justify-content: flex-end;
		min-height: 24px;
		margin-bottom: 2px;
		font-size: 12px;
		color: var(--text-muted);
	}
	.author {
		font-size: 13px;
		font-weight: 600;
		color: var(--text-primary);
	}
	.body {
		box-sizing: border-box;
		max-width: 85%;
		margin: 0;
		padding: 7px 12px;
		border: 1px dashed color-mix(in srgb, var(--accent-primary) 40%, transparent);
		border-radius: 14px 14px 4px 14px;
		background: color-mix(in srgb, var(--accent-primary) 16%, transparent);
		color: var(--text-primary);
		font-size: 14px;
		line-height: 1.5;
		white-space: pre-wrap;
		overflow-wrap: anywhere;
		opacity: 0.75;
	}
	.bubble:not(.head) .body { border-top-right-radius: 4px; }
	.failed .body {
		border-color: var(--accent-red);
		opacity: 1;
	}
	.state-row {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		justify-content: flex-end;
		gap: var(--space-2);
		margin-top: 4px;
	}
	.state {
		margin: 0;
		font-size: 12px;
		color: var(--text-muted);
	}
	.failed .state { color: var(--accent-red); }
</style>
