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
 .console-layout.desktop { min-height: 0; }
 .console-layout { background: var(--bg-primary); }
 .console-main { max-width: 960px; margin: 0 auto; padding: var(--space-8) var(--space-6); }
 .console-main.wide { max-width: 1240px; }
 @media (max-width: 640px) { .console-main { padding: var(--space-6) var(--space-4); } }
</style>
