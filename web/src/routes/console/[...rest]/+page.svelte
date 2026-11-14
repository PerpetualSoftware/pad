<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { legacyConsoleTarget } from '$lib/utils/legacyConsoleRedirect';

	onMount(() => {
		const { pathname, search, hash } = page.url;
		void goto(legacyConsoleTarget(pathname, search, hash), { replaceState: true });
	});
</script>

<p>Redirecting...</p>
