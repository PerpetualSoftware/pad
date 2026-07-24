<script lang="ts">
	import type { Snippet } from 'svelte';

	interface Props {
		title: string;
		/** Optional leading icon/emoji. */
		icon?: string;
		/** Count pill after the title (rendered when >= 0). */
		count?: number;
		/** One-line description under the title row. */
		description?: string;
		/** Right-aligned actions (buttons, filters). */
		actions?: Snippet;
		/** Extra rows under the header (tabs, filter bars). */
		children?: Snippet;
	}

	let { title, icon, count, description, actions, children }: Props = $props();
</script>

<header class="page-header">
	<div class="header-row">
		<h1>
			{#if icon}<span class="header-icon" aria-hidden="true">{icon}</span>{/if}
			{title}
			{#if count !== undefined && count >= 0}<span class="count">{count}</span>{/if}
		</h1>
		{#if actions}
			<div class="header-actions">{@render actions()}</div>
		{/if}
	</div>
	{#if description}
		<p class="header-desc">{description}</p>
	{/if}
	{#if children}
		{@render children()}
	{/if}
</header>

<style>
	.page-header {
		margin-bottom: var(--space-6);
	}

	.header-row {
		display: flex;
		align-items: center;
		gap: var(--space-3);
	}

	h1 {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		margin: 0;
		font-size: 1.45em;
		font-weight: 700;
		letter-spacing: -0.01em;
		min-width: 0;
	}

	.header-icon {
		font-size: 0.85em;
	}

	.count {
		font-size: 0.55em;
		font-weight: 600;
		color: var(--text-muted);
		background: var(--bg-tertiary);
		border-radius: 99px;
		padding: 2px 9px;
		font-variant-numeric: tabular-nums;
	}

	.header-actions {
		margin-left: auto;
		display: flex;
		align-items: center;
		gap: var(--space-2);
		flex: 0 0 auto;
	}

	.header-desc {
		margin: var(--space-2) 0 0;
		color: var(--text-secondary);
		font-size: 0.95em;
		max-width: 70ch;
	}
</style>
