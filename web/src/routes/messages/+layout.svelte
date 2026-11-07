<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { onMount } from 'svelte';
	import ChatOff from '$lib/components/chat/ChatOff.svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { chatSetting } from '$lib/stores/chatSetting.svelte';
	import { viewport } from '$lib/stores/breakpoint.svelte';

	let { children } = $props();
	let ready = $state(false);
	// A thread owns its scroll container, so it gets a viewport-height column
	// (dvh follows the phone's browser chrome) instead of a growing document.
	let isThread = $derived(page.route?.id === '/messages/[n]');

	// On a phone this page is standalone and is the document's <main>. On a desktop
	// it renders inside the shell's <main>, so it is a plain region there (one main
	// landmark per page).
	let standalone = $derived(viewport.isMobile);

	// A sign-out, or a swap to a signed-out identity, while /messages is open sends
	// the visitor to /login (TASK-4044 2b-ii, row J). Read-only on authStore.
	onMount(() =>
		authStore.onIdentityChange(() => {
			if (!authStore.authenticated) goto('/login', { replaceState: true });
		})
	);

	// The root layout gives a phone /messages no app shell, so signed-out visitors
	// are sent to /login here, as the console layout does. A guest is not redirected:
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
	<svelte:element
		this={standalone ? 'main' : 'div'}
		class="messages-main"
		class:standalone
		class:thread={isThread && chatSetting.enabled}
	>
		<!-- Off is a display preference (TASK-4315): the URL still answers, and says why. -->
		{#if chatSetting.enabled}
			{@render children()}
		{:else}
			<ChatOff />
		{/if}
	</svelte:element>
{/if}

<style>
	.messages-main {
		max-width: 960px;
		margin: 0 auto;
		padding: var(--space-8) var(--space-6);
		min-height: 100vh;
		background: var(--bg-primary);
	}

	/* Under the shell the page scrolls inside the shell's own main. */
	.messages-main:not(.standalone) {
		min-height: 100%;
	}

	.messages-main.thread {
		display: flex;
		flex-direction: column;
		height: 100%;
		min-height: 0;
		box-sizing: border-box;
		padding-bottom: calc(var(--space-4) + env(safe-area-inset-bottom));
	}

	.messages-main.thread.standalone {
		height: 100dvh;
	}

	@media (max-width: 640px) {
		.messages-main {
			padding: var(--space-6) var(--space-4);
		}
	}
</style>
