<script lang="ts">
	import { onDestroy } from 'svelte';
	let { log }: { log: (s: string) => void } = $props();
	onDestroy(() => log('child-onDestroy'));
</script>

<div>child</div>
