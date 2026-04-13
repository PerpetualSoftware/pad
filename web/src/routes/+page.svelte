<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';

	onMount(async () => {
		goto('/console', { replaceState: true });
	});
</script>

<div class="welcome">
	<p>Redirecting...</p>
</div>

<style>
	.welcome {
		display: flex;
		flex-direction: column;
		align-items: center;
		justify-content: center;
		height: 100%;
		text-align: center;
		color: var(--text-secondary);
		gap: var(--space-3);
	}
</style>
