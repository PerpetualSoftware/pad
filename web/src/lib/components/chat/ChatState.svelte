<script lang="ts">
	import type { Snippet } from 'svelte';

	// The one centred state of the chat surfaces (TASK-4515): Chat off, no agents, an empty
	// thread, a conversation not found or failed to load. A decorative icon tile, a title,
	// the body (children) and the actions. `level` is the title's heading level; without one
	// the title is plain text (the empty thread sits under the header's h1 and is not a section).
	let {
		heading,
		level,
		tone = 'muted',
		label,
		children,
		actions
	}: {
		heading: string;
		level?: 1 | 2;
		tone?: 'muted' | 'accent';
		/** Names the state as a region (the no-agents help keeps "No agents yet"). */
		label?: string;
		children?: Snippet;
		actions?: Snippet;
	} = $props();
</script>

<section class="chat-state" aria-label={label}>
	<div class="tile {tone}" aria-hidden="true">
		<svg viewBox="0 0 28 28" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linejoin="round"
			><path d="M5 6.5h18V19h-8.5L9.5 23v-4H5z" /></svg
		>
	</div>
	{#if level === 1}
		<h1 class="title">{heading}</h1>
	{:else if level === 2}
		<h2 class="title">{heading}</h2>
	{:else}
		<p class="title">{heading}</p>
	{/if}
	{@render children?.()}
	{#if actions}
		<div class="actions">{@render actions()}</div>
	{/if}
</section>

<style>
	.chat-state {
		margin: auto;
		box-sizing: border-box;
		width: 100%;
		max-width: 400px;
		padding: var(--space-8) var(--space-5);
		display: flex;
		flex-direction: column;
		align-items: center;
		text-align: center;
		gap: var(--space-3);
	}
	.tile {
		width: 56px;
		height: 56px;
		border-radius: 16px;
		display: flex;
		align-items: center;
		justify-content: center;
		flex-shrink: 0;
	}
	.tile.muted {
		background: var(--bg-tertiary);
		color: var(--text-muted);
	}
	.tile.accent {
		background: color-mix(in srgb, var(--accent-primary) 14%, transparent);
		color: var(--accent-primary);
	}
	.tile svg {
		width: 28px;
		height: 28px;
	}
	.title {
		margin: 0;
		font-size: 18px;
		font-weight: 650;
		letter-spacing: -0.01em;
		color: var(--text-primary);
	}
	p.title {
		font-size: 15px;
		font-weight: 600;
	}
	/* :where() adds nothing for the paragraph, so a caller's own p.class rule wins. */
	.chat-state :global(:where(p)) {
		margin: 0;
		font-size: 14px;
		color: var(--text-secondary);
	}
	.actions {
		display: flex;
		flex-direction: column;
		align-items: center;
		gap: var(--space-3);
		margin-top: var(--space-2);
	}
</style>
