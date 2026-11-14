<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';

	onMount(() => {
		void goto('/settings/account' + window.location.search + window.location.hash, { replaceState: true });
	});
</script>

<p>Redirecting...</p>
