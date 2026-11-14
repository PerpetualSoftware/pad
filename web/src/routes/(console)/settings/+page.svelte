<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';

	onMount(() => {
		void goto('/settings/account' + page.url.search + page.url.hash, { replaceState: true });
	});
</script>

<p>Redirecting...</p>
