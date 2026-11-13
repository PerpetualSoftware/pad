<script lang="ts">
	// The "N new" line at the read marker as of open (TASK-4044 PR 3a). `more`
	// is true when rows beyond the window are unread too, so the count is a floor.
	// `rest` carries an attachment from the thread, which watches whether the line is in view.
	let { count, more = false, ...rest }: { count: number; more?: boolean } & Record<symbol, unknown> = $props();
</script>

<div {...rest} class="divider" role="separator" id="unread-divider">
	<span>{count}{more ? '+' : ''} new</span>
</div>

<style>
	/* The day separator's shape, in the accent (TASK-4515). */
	.divider {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		margin: var(--space-3) 0 var(--space-1);
		color: var(--accent-primary);
		font-size: 12px;
		font-weight: 600;
	}
	.divider::before,
	.divider::after {
		content: '';
		flex: 1;
		border-top: 1px solid currentColor;
		opacity: 0.45;
	}
</style>
