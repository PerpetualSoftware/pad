<script lang="ts">
 import { page } from '$app/state';
 import { uiStore } from '$lib/stores/ui.svelte';
 import { homeOverview } from '$lib/stores/homeOverview.svelte';
 import { coveredPage } from '$lib/stores/coveredPage.svelte';
 import AccountButton from './AccountButton.svelte';
</script>
{#if uiStore.isMobile}
 <header class="you-topbar" inert={coveredPage.active}>
  <a href="/" aria-label="Pad Home" class="logo"><img src="/pad-mark.svg" width="22" height="22" alt="" /></a>
  <nav aria-label="You pages">
   {#each [{ label: 'Home', href: '/' }, { label: 'Needs you', href: '/needs' }, { label: 'Agenda', href: '/agenda' }] as tab}
    <a href={tab.href} aria-current={page.url.pathname === tab.href ? 'page' : undefined}>
     {tab.label}
     {#if tab.href === '/needs' && homeOverview.needs?.total}<span class="needs-count">{homeOverview.needs.total}</span>{/if}
    </a>
   {/each}
  </nav>
  <AccountButton />
 </header>
{/if}
<style>
 .you-topbar { display: flex; align-items: center; gap: var(--space-2); min-height: calc(var(--topbar-height) + env(safe-area-inset-top, 0px)); padding: env(safe-area-inset-top, 0px) var(--space-3) 0; background: var(--bg-secondary); border-bottom: 1px solid var(--border); }
 .logo { display: flex; flex-shrink: 0; }
 nav { display: flex; align-self: stretch; flex: 1; min-width: 0; }
 nav a { flex: 1; display: flex; align-items: center; justify-content: center; gap: var(--space-1); padding: var(--space-2); border-bottom: 2px solid transparent; color: var(--text-secondary); text-decoration: none; font-size: 1rem; font-weight: 600; white-space: nowrap; }
 nav a[aria-current="page"] { color: var(--text-primary); border-bottom-color: var(--accent-primary); }
 .needs-count { border-radius: 99px; padding: 1px 5px; font-size: .7rem; background: color-mix(in srgb, var(--accent-amber) 14%, transparent); color: var(--accent-amber); }
</style>
