<script lang="ts">
 import { onDestroy, untrack } from 'svelte';
 import { api } from '$lib/api/client';
 import { authStore } from '$lib/stores/auth.svelte';
 import { browserZone } from '$lib/timezone';
 import { describeGate, describeLinkOffset, type ItemCommitmentSnapshot } from '$lib/commitment';
 import { formatItemRef, itemUrlId, type CommitmentGateView, type Item, type PaneTarget } from '$lib/types';
 import { shouldOpenInPane } from '$lib/components/collections/itemCardClick';

 interface Props {
  wsSlug: string;
  username: string;
  itemId: string;
  itemSlug: string;
  snapshot: ItemCommitmentSnapshot | null;
  onOpenTarget?: (target: PaneTarget) => void;
 }
 let { wsSlug, username, itemId, itemSlug, snapshot, onOpenTarget }: Props = $props();
 const zone = $derived(authStore.user?.timezone || browserZone() || 'UTC');
 let loaded = $state.raw<{ source: ItemCommitmentSnapshot; gates: CommitmentGateView; items: Record<string, Item> } | null>(null);
 let failed = $state.raw<ItemCommitmentSnapshot | null>(null);
 let generation = 0;
 let disposed = false;
 const source = $derived(snapshot && snapshot.wsSlug === wsSlug && snapshot.itemId === itemId && snapshot.itemSlug === itemSlug
  && snapshot.authEpoch === authStore.identityEpoch && snapshot.zone === zone ? snapshot : null);
 const result = $derived(loaded?.source === source ? loaded : null);
 const links = $derived(result?.source.view?.links ?? []);
 const gates = $derived(result?.gates);
 const hasGates = $derived(gates && (gates.gates.length > 0 || gates.state !== 'released'));

 async function load() {
  const ticket = ++generation;
  const isSameIdentity = authStore.identityFence();
  const target = source;
  loaded = null;
  failed = null;
  if (!target || target.view === undefined) return;
  const current = () => !disposed && ticket === generation && target === source;
  try {
   const gateView = target.view === null ? await api.getCommitmentGates(target.wsSlug, target.itemSlug) : target.view.gates;
   if (!isSameIdentity() || !current()) return;
   const ids = new Set([
    ...(target.view?.links ?? []).map(link => link.child_item_id === target.itemId ? link.parent_item_id : link.child_item_id),
    ...gateView.gates.map(gate => gate.blocker_item_id)
   ]);
   const entries = await Promise.all([...ids].map(async id => {
    try { return [id, await api.items.get(target.wsSlug, id)] as const; }
    catch { return null; }
   }));
   if (!isSameIdentity() || !current()) return;
   loaded = { source: target, gates: gateView, items: Object.fromEntries(entries.filter(entry => entry !== null)) };
  } catch {
   if (!isSameIdentity() || !current()) return;
   failed = target;
  }
 }
 $effect(() => { void source; untrack(() => { void load(); }); });
 onDestroy(() => { disposed = true; generation++; });

 function named(id: string) {
  const other = result?.items[id];
  const ref = other && formatItemRef(other);
  if (!other || !ref) return null;
  return { label: `${ref} · ${other.title}`, ref, slug: other.slug, collectionSlug: other.collection_slug,
   href: other.collection_slug ? `/${[username, wsSlug, other.collection_slug, itemUrlId(other)].map(encodeURIComponent).join('/')}` : undefined };
 }
 function navigate(event: MouseEvent, target: PaneTarget) {
  if (!shouldOpenInPane(event, !!onOpenTarget)) return;
  event.preventDefault();
  onOpenTarget?.(target);
 }
</script>

{#snippet sentence(text: string, id: string)}
 {@const parts = text.split('\u0000')}
 {@const other = named(id)}
 {parts[0]}{#if other?.href}<a href={other.href} onclick={(event) => navigate(event, other)}>{other.label}</a>{:else}<span>{other?.label ?? 'Linked item'}</span>{/if}{parts[1]}
{/snippet}

{#if failed && failed === source}
 <section class="commitment-links" aria-label="Date relationships"><p role="status">Date unavailable</p><button onclick={() => load()}>Retry</button></section>
{:else if links.length || hasGates}
 <section class="commitment-links" aria-label="Date relationships">
  <h3>Date relationships</h3>
  {#each links as link (link.id)}
   <p>{@render sentence(describeLinkOffset(link, itemId, '\u0000'), link.child_item_id === itemId ? link.parent_item_id : link.child_item_id)}</p>
  {/each}
  {#if hasGates && gates}
   <p class="aggregate">{describeGate(gates)}</p>
   {#each gates.gates as gate (gate.id)}<p>{@render sentence(describeGate(gate, '\u0000'), gate.blocker_item_id)}</p>{/each}
  {/if}
 </section>
{/if}

<style>
 .commitment-links { min-width: 0; padding: var(--space-3) 0; color: var(--text-primary); font-size: 0.82em; overflow-wrap: anywhere; }
 h3 { font-size: 1em; color: var(--text-secondary); font-weight: 600; margin: 0 0 var(--space-2); }
 p { margin: var(--space-2) 0; }
 a { color: var(--accent-blue); text-decoration: none; }
 a:hover { text-decoration: underline; }
 .aggregate { font-weight: 500; }
 button { min-height: 44px; cursor: pointer; color: var(--text-primary); background: var(--bg-secondary); border: 1px solid var(--border); border-radius: var(--radius-sm); padding: var(--space-2) var(--space-3); }
</style>
