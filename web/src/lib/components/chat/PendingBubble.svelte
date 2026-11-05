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
	<header><strong>You</strong><span>Human</span></header>
	<p class="body">{pending.body}</p>
	<!-- Mounted for the bubble's life, only its text changes (r5 H5). -->
	<p class="state" role="status">{pending.status === 'sending' ? 'Sending…' : 'Failed to send.'}</p>
	{#if pending.status === 'failed'}
		<p class="state">
			<button type="button" onclick={() => onRetry(pending.op_key)}>Retry</button>
			<button type="button" onclick={() => onDiscard(pending.op_key)}>Discard</button>
		</p>
	{/if}
</article>

<style>
	.bubble {
		box-sizing: border-box;
		width: 100%;
		min-width: 0;
		align-self: stretch;
		padding: var(--space-2) var(--space-3);
		border: 1px dashed var(--border);
		background: var(--bg-hover);
		opacity: 0.85;
	}
	.bubble.failed {
		border-color: var(--accent-red, var(--border));
		opacity: 1;
	}
	header { display: flex; align-items: center; gap: var(--space-2); min-height: 44px; font-size: 0.75rem; color: var(--text-secondary); }
	button { min-height: 44px; min-width: 44px; }
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
