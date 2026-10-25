<script lang="ts">
	let { count }: { count: number } = $props();
</script>

{#if count > 0}
	<span class="unread-badge" aria-label="{count} unread">{count > 99 ? '99+' : count}</span>
{/if}

<style>
	.unread-badge {
		display: inline-block;
		min-width: 1.25rem;
		padding: 0 var(--space-1);
		border-radius: 999px;
		background: var(--accent-blue);
		color: #fff;
		font-size: 0.7rem;
		font-weight: 600;
		line-height: 1.25rem;
		text-align: center;
	}
</style>
