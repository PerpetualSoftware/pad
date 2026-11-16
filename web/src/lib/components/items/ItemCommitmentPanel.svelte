<script lang="ts">
 import { onDestroy, onMount, untrack } from 'svelte';
 import { api } from '$lib/api/client';
 import type { Collection, Item } from '$lib/types';
 import { authStore } from '$lib/stores/auth.svelte';
 import { sseService } from '$lib/services/sse.svelte';
 import { browserZone } from '$lib/timezone';
 import { childState } from '$lib/collections/childProgress';
 import { commitmentDisplayState, describeRecurrence, type ItemCommitmentSnapshot } from '$lib/commitment';
 import { acknowledgedText, dateTime, deadlinePill, forecastText, isDateOnly, roleText } from '$lib/calendar/presentation';
 import StateMark from '$lib/components/calendar/StateMark.svelte';

 interface Props {
  wsSlug: string;
  itemSlug: string;
  item: Item;
  collection: Collection | null;
  onSnapshot: (snapshot: ItemCommitmentSnapshot) => void;
 }
 let { wsSlug, itemSlug, item, collection, onSnapshot }: Props = $props();
 const itemId = $derived(item.id);
 const zone = $derived(authStore.user?.timezone || browserZone() || 'UTC');
 let snapshot = $state.raw<ItemCommitmentSnapshot | null>(null);
 let failed = $state(false);
 let now = $state(new Date(Date.now()));
 let generation = 0;
 let disposed = false;
 let refreshQueued = false;
 let requestedIdentity = '';

 function current(s: ItemCommitmentSnapshot): boolean {
  return !disposed && s.wsSlug === wsSlug && s.itemId === item.id && s.itemSlug === itemSlug
   && s.authEpoch === authStore.identityEpoch && s.zone === zone;
 }
 const view = $derived(snapshot && current(snapshot) ? snapshot.view : undefined);
 const unavailable = $derived(!!snapshot && current(snapshot) && failed);
 const display = $derived(view ? commitmentDisplayState({ ...view, itemClosed: childState(item, collection ?? undefined) !== 'open' }, now) : null);

 async function load() {
  const ticket = ++generation;
  const isSameIdentity = authStore.identityFence();
  const target: ItemCommitmentSnapshot = { wsSlug, itemId: item.id, itemSlug, authEpoch: authStore.identityEpoch, zone, view: undefined };
  failed = false;
  // A background refresh of the same identity keeps the shown view until its
  // answer arrives, so an edit to the item doesn't blank the section.
  if (!(snapshot && current(snapshot) && snapshot.view !== undefined)) {
   snapshot = target;
   onSnapshot(target);
  }
  try {
   const loaded = await api.getCommitment(target.wsSlug, target.itemSlug);
   if (!isSameIdentity() || ticket !== generation || !current(target)) return;
   snapshot = { ...target, view: loaded };
   onSnapshot(snapshot);
  } catch (error) {
   if (!isSameIdentity() || ticket !== generation || !current(target)) return;
   // The client preserves the server's not_found code (HTTP 404), not HTTP status.
   if ((error as { code?: string })?.code === 'not_found') {
    snapshot = { ...target, view: null };
    onSnapshot(snapshot);
   } else failed = true;
  }
 }

 $effect(() => {
  const identity = JSON.stringify([wsSlug, itemId, itemSlug, authStore.identityEpoch, zone]);
  if (identity === requestedIdentity) return;
  requestedIdentity = identity;
  untrack(() => { void load(); });
 });
 onMount(() => {
  const unsubscribe = sseService.onItemEvent(event => {
   if (event.type !== 'item_updated' || event.item_id !== item.id || refreshQueued) return;
   refreshQueued = true;
   const ticket = generation;
   queueMicrotask(() => {
    refreshQueued = false;
    if (!disposed && ticket === generation) void load();
   });
  });
  const timer = setInterval(() => { now = new Date(Date.now()); }, 60_000);
  return () => { unsubscribe(); clearInterval(timer); };
 });
 onDestroy(() => { disposed = true; generation++; });
</script>

{#if view || unavailable}
 <section class="commitment-panel" aria-label="When">
  <h3>When</h3>
  {#if view && display}
   {#if view.roles.target}
    <div class="date-row primary"><span class="label">Target</span><span>{roleText(view.roles.target, zone)}</span></div>
   {:else if view.next_occurrence}
    <div class="date-row primary"><span class="label">Next occurrence</span><span>{isDateOnly(view.dtstart) ? roleText(view.next_occurrence.wall.slice(0, 10), zone) : dateTime(view.next_occurrence.at, zone)}</span></div>
   {/if}
   <div class="date-row"><span class="label">Repeats</span><span class="recurrence">{describeRecurrence(view)}</span></div>
   {#if view.rdates.length}<details><summary>Additional dates</summary>{#each view.rdates as date}<div>{date}</div>{/each}</details>{/if}
   {#if view.exdates.length}<details><summary>Excluded dates</summary>{#each view.exdates as date}<div>{date}</div>{/each}</details>{/if}
   {#if view.roles.deadline}
    {@const pill = deadlinePill(view.roles.deadline, view.roles.deadline_behavior, zone, now)}
    <div class="date-row"><span class="label">Deadline</span><span title={pill.text}>{roleText(view.roles.deadline, zone)}{#if view.roles.deadline_behavior}<span class="behavior">{view.roles.deadline_behavior === 'lapses' ? 'Lapses' : 'Still owed'}</span>{/if}</span></div>
   {/if}
   {#if view.roles.earliest}<div class="date-row"><span class="label">Earliest</span><span>{roleText(view.roles.earliest, zone)}</span></div>{/if}
   {#if view.roles.occupied_until}<div class="date-row"><span class="label">Occupied end</span><span>{roleText(view.roles.occupied_until, zone)}</span></div>{/if}
   {#if view.forecast}<p>{forecastText(view.forecast, zone)}</p>{/if}
   <p class="state"><StateMark state={display.mark} label={display.primary} /><span>{display.primary}</span></p>
   {#each display.annotations as annotation}<p>{annotation}</p>{/each}
   {#if display.acknowledgedAt || display.acknowledgedBy}<p>{acknowledgedText(display.acknowledgedAt, display.acknowledgedBy, zone, now)}</p>{/if}
   {#if display.unavailable}<button onclick={() => load()}>Retry</button>{/if}
  {:else}
   <p role="status">Date unavailable</p><button onclick={() => load()}>Retry</button>
  {/if}
 </section>
{/if}

<style>
 .commitment-panel { grid-column: 1 / -1; min-width: 0; container-type: inline-size; padding-bottom: var(--space-3); color: var(--text-primary); font-size: 0.82em; overflow-wrap: anywhere; }
 h3 { font-size: calc(0.7em / 0.82); font-weight: 600; text-transform: uppercase; letter-spacing: 0.05em; color: var(--text-muted); padding: var(--space-2) 0; margin: 0 0 var(--space-1); }
 .date-row { display: flex; gap: var(--space-3); padding: var(--space-2) 0; }
 .label { width: 90px; flex-shrink: 0; color: var(--text-secondary); font-weight: 500; }
 .primary { font-weight: 500; }
 p { margin: var(--space-2) 0; }
 .recurrence, details { color: var(--text-secondary); }
 .behavior { display: inline-block; margin-left: var(--space-2); }
 .state { display: flex; align-items: center; gap: var(--space-2); }
 button, summary { min-height: 44px; cursor: pointer; }
 summary { align-content: center; }
 button { color: var(--text-primary); background: var(--bg-secondary); border: 1px solid var(--border); border-radius: var(--radius-sm); padding: var(--space-2) var(--space-3); }
 @container (max-width: 360px) { .date-row { flex-direction: column; gap: var(--space-1); } .label { width: auto; } }
 @media (max-width: 480px) { .date-row { flex-direction: column; gap: var(--space-1); } .label { width: auto; } }
</style>
