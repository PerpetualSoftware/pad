<script lang="ts">
	let { count }: { count: number } = $props();
</script>

{#if count > 0}
	<span class="unread-badge" aria-label="{count} unread">{count > 99 ? '99+' : count}</span>
{/if}

<style>
	/* --accent-primary-strong under --text-on-accent holds AA in both themes (TASK-4515). */
	.unread-badge {
		display: inline-block;
		box-sizing: border-box;
		min-width: 20px;
		padding: 0 5px;
		border-radius: 999px;
		background: var(--accent-primary-strong);
		color: var(--text-on-accent);
		font-size: 11px;
		font-weight: 600;
		line-height: 20px;
		text-align: center;
	}
</style>
