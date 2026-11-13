<script lang="ts">
	// The line before the first row of each local day (TASK-4515): "Today", "Yesterday", "Wed, Nov 11".
	let { label }: { label: string } = $props();
</script>

<div class="day" role="separator" aria-label={label}>
	<span>{label}</span>
</div>

<style>
	.day {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		margin: var(--space-4) 0 var(--space-2);
		font-size: 12px;
		font-weight: 600;
		color: var(--text-muted);
	}
	.day:first-child {
		margin-top: 0;
	}
	.day::before,
	.day::after {
		content: '';
		flex: 1;
		border-top: 1px solid var(--border-subtle);
	}
</style>
