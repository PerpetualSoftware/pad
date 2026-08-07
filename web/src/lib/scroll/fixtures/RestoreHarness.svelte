<script lang="ts">
	// Test-only wrapper: `createScrollRestoration` calls `onDestroy`, which needs a
	// real component context (`$effect.root` does not provide one), so the restore
	// integration tests (TASK-2457) mount this and drive the returned snapshot.
	import { createScrollRestoration } from '../restore.svelte';
	import type { Snapshot } from '@sveltejs/kit';

	let {
		ready,
		scrollTarget,
		expose,
	}: {
		ready: () => boolean;
		scrollTarget: () => HTMLElement | Window | null;
		expose: (snap: Snapshot<number>) => void;
	} = $props();

	// Wrap the prop functions so the factory reads the current prop on each call
	// rather than capturing the init value (state_referenced_locally).
	const restoration = createScrollRestoration({
		ready: () => ready(),
		scrollTarget: () => scrollTarget(),
	});
	$effect(() => {
		expose(restoration.snapshot);
	});
</script>
