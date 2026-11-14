<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { legacyConsoleTarget } from '$lib/utils/legacyConsoleRedirect';

	onMount(() => {
		const { pathname, search, hash } = window.location;
		void goto(legacyConsoleTarget(pathname, search, hash), { replaceState: true });
	});
</script>

<p>Redirecting...</p>
