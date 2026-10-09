<script lang="ts">
	// Stands in for ItemDetail in PaneHost.lazyDetail.svelte.test.ts (TASK-2226).
	let { ref }: { ref: string } = $props();
</script>

<p data-testid="item-detail-stub">{ref}</p>
