<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { onMount } from 'svelte';
	import ChatOff from '$lib/components/chat/ChatOff.svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { chatSetting } from '$lib/stores/chatSetting.svelte';
	import { viewport } from '$lib/stores/breakpoint.svelte';
	import { useViewportFit } from '$lib/chat/viewportOwner.svelte';

	let { children } = $props();
	let ready = $state(false);
	// A thread owns its scroll container, so it gets a viewport-height column
	// (dvh follows the phone's browser chrome) instead of a growing document.
	let isThread = $derived(page.route?.id === '/messages/[n]');

	// On a phone this page is standalone and is the document's <main>. On a desktop
	// it renders inside the shell's <main>, so it is a plain region there (one main
	// landmark per page).
	let standalone = $derived(viewport.isMobile);

	// A standalone phone thread fits the visual viewport while the soft keyboard is up,
	// so its composer is never covered (the same owner the Chat sheet uses). Under the
	// shell (iPad in route mode) there is no owner: the browser scrolls the box into view.
	const fit = useViewportFit();
	let fitted = $derived(standalone && isThread && chatSetting.enabled ? fit.box : null);

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
		style:position={fitted ? 'fixed' : undefined}
		style:top={fitted ? `${fitted.top}px` : undefined}
		style:left={fitted ? '0' : undefined}
		style:right={fitted ? '0' : undefined}
		style:height={fitted ? `${fitted.height}px` : undefined}
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
		/* A reading measure (TASK-4515). */
		max-width: 760px;
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
		padding: var(--space-4) var(--space-6) calc(var(--space-4) + env(safe-area-inset-bottom));
	}

	/* A standalone phone thread is edge to edge: its header, scroller and composer pad themselves. */
	.messages-main.thread.standalone {
		padding: 0 0 env(safe-area-inset-bottom);
	}

	/* The bar below (GlobalBottomBar) takes its height, and the body's bottom padding
	   under it, while it is shown; with the keyboard up the bar is gone. */
	.messages-main.thread.standalone {
		height: 100dvh;
	}
	:global(body.has-bottom-nav) .messages-main.thread.standalone {
		height: calc(100dvh - var(--bottom-nav-height) - env(safe-area-inset-bottom, 0px));
	}

	@media (max-width: 640px) {
		.messages-main {
			padding: var(--space-6) var(--space-4);
		}
	}
</style>
