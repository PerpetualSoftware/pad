<script lang="ts">
	// The "N new" line at the read marker as of open (TASK-4044 PR 3a). `more`
	// is true when rows beyond the window are unread too, so the count is a floor.
	let { count, more = false }: { count: number; more?: boolean } = $props();
</script>

<div class="divider" role="separator" id="unread-divider">
	<span>{count}{more ? '+' : ''} new</span>
</div>

<style>
	.divider {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		color: var(--accent-blue, var(--text-secondary));
		font-size: 0.75rem;
		font-weight: 600;
	}
	.divider::before,
	.divider::after {
		content: '';
		flex: 1;
		border-top: 1px solid currentColor;
		opacity: 0.5;
	}
</style>
