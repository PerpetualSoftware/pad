<script lang="ts">
	import { goto } from '$app/navigation';
	import { onMount } from 'svelte';
	import { authStore } from '$lib/stores/auth.svelte';

	let { children } = $props();
	let ready = $state(false);

	// A sign-out, or a swap to a signed-out identity, while /messages is open sends
	// the visitor to /login (TASK-4044 2b-ii, row J). Read-only on authStore.
	onMount(() =>
		authStore.onIdentityChange(() => {
			if (!authStore.authenticated) goto('/login', { replaceState: true });
		})
	);

	// The root layout gives /messages no app shell, so signed-out visitors are
	// sent to /login here, as the console layout does. A guest is not redirected:
	// the API is owner-scoped and the page leaks nothing.
	onMount(async () => {
		try {
			const session = await authStore.load();
			if (!session?.authenticated) {
				goto('/login', { replaceState: true });
				return;
			}
		} catch {
			goto('/login', { replaceState: true });
			return;
		}
		ready = true;
	});
</script>

{#if ready}
	<main class="messages-main">
		{@render children()}
	</main>
{/if}

<style>
	.messages-main {
		max-width: 960px;
		margin: 0 auto;
		padding: var(--space-8) var(--space-6);
		min-height: 100vh;
		background: var(--bg-primary);
	}

	@media (max-width: 640px) {
		.messages-main {
			padding: var(--space-6) var(--space-4);
		}
	}
</style>
