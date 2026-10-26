<script lang="ts">
	import type { PendingSend } from '$lib/chat/model';

	// A send the server has not confirmed (TASK-4044 PR 3b): shown at once as
	// "Sending…", and as "Failed to send" with Retry (the SAME op_key) and Discard
	// when it did not go through. It has no message menu: it is not a message yet.
	let {
		pending,
		onRetry,
		onDiscard
	}: { pending: PendingSend; onRetry: (opKey: string) => void; onDiscard: (opKey: string) => void } =
		$props();
</script>

<article class="bubble mine pending" class:failed={pending.status === 'failed'} data-op-key={pending.op_key}>
	<p class="body">{pending.body}</p>
	{#if pending.status === 'sending'}
		<p class="state" role="status">Sending…</p>
	{:else}
		<p class="state" role="alert">
			Failed to send.
			<button type="button" onclick={() => onRetry(pending.op_key)}>Retry</button>
			<button type="button" onclick={() => onDiscard(pending.op_key)}>Discard</button>
		</p>
	{/if}
</article>

<style>
	.bubble {
		max-width: min(36rem, 85%);
		align-self: flex-end;
		padding: var(--space-2) var(--space-3);
		border: 1px dashed var(--border);
		border-radius: var(--radius-lg);
		background: var(--bg-hover);
		opacity: 0.85;
	}
	.bubble.failed {
		border-color: var(--accent-red, var(--border));
		opacity: 1;
	}
	.body {
		margin: 0;
		white-space: pre-wrap;
		overflow-wrap: anywhere;
	}
	.state {
		margin: var(--space-1) 0 0;
		font-size: 0.75rem;
		color: var(--text-secondary);
	}
</style>
