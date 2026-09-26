<script lang="ts">
	// A muted dot on a row whose body snippet the server marked as behind
	// (BUG-3050 U3). Rendered on MARKED rows only; callers gate it.
	import { STALE_BODY_NOTICE } from '$lib/items/staleBody';
</script>

<span
	class="stale-body-dot"
	role="img"
	aria-label={STALE_BODY_NOTICE}
	title={STALE_BODY_NOTICE}
	data-testid="stale-body-dot"
></span>

<style>
	.stale-body-dot {
		display: inline-block;
		flex-shrink: 0;
		width: 6px;
		height: 6px;
		margin: 0 var(--space-1);
		border-radius: 50%;
		background: var(--text-muted);
		vertical-align: middle;
	}
</style>
