<script lang="ts">
	// The one-line notice above a body the server marked as behind (BUG-3050 U3).
	import { STALE_BODY_NOTICE } from '$lib/items/staleBody';
</script>

<p class="stale-body-notice" role="note" data-testid="stale-body-notice">{STALE_BODY_NOTICE}</p>

<style>
	.stale-body-notice {
		margin: 0 0 var(--space-3);
		font-size: 0.85em;
		color: var(--text-secondary);
	}
</style>
