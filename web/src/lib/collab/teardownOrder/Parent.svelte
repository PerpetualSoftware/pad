<script lang="ts">
	import Child from './Child.svelte';
	let { log }: { log: (s: string) => void } = $props();
	// Mirrors ItemDetail's collab $effect: a top-level $effect with a
	// cleanup return, alongside a child component (the <Editor>) that
	// registers onDestroy. The question TASK-2117 hinges on: on unmount,
	// does this $effect cleanup run BEFORE or AFTER the child's onDestroy?
	$effect(() => {
		return () => log('parent-effect-cleanup');
	});
</script>

<Child {log} />
