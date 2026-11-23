<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { authStore } from '$lib/stores/auth.svelte';
	import { onMount } from 'svelte';
	import { viewport } from '$lib/stores/breakpoint.svelte';
	let { children } = $props();
	let ready = $state(false);
	onMount(async () => {
		if (!viewport.isMobile) { ready = true; return; }
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
	const wide = $derived(page.url.pathname === '/' || page.url.pathname === '/agenda' || page.url.pathname.startsWith('/agenda/'));
</script>
{#if ready}
	<div class="console-layout" class:desktop={!viewport.isMobile}>
		<div class="console-main" class:wide>{@render children()}</div>
	</div>
{/if}
<style>
	/* Phone: fill the viewport below the in-flow You bar (border-box, so its border is inside
	   its min-height), and give back the bottom-bar reserve that pads <main>, so a short page
	   fills the screen without scrolling. */
	.console-layout { min-height: calc(100dvh - var(--topbar-height) - env(safe-area-inset-top, 0px)); background: var(--bg-primary); }
	:global(body.has-bottom-nav) .console-layout:not(.desktop) { min-height: calc(100dvh - var(--topbar-height) - env(safe-area-inset-top, 0px) - var(--bottom-nav-height) - env(safe-area-inset-bottom, 0px)); }
	.console-layout.desktop { min-height: 0; }
	.console-main { max-width: 960px; margin: 0 auto; padding: var(--space-8) var(--space-6); }
	.console-main.wide { max-width: 1240px; }
	@media (max-width: 768px) { .console-main { padding: var(--space-5) var(--space-3); } }
</style>
