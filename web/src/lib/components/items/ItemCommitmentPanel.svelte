<script lang="ts">
 import { onDestroy, onMount, untrack } from 'svelte';
 import { api } from '$lib/api/client';
 import { formatItemRef, type Collection, type Item } from '$lib/types';
 import { authStore } from '$lib/stores/auth.svelte';
 import { sseService } from '$lib/services/sse.svelte';
 import { browserZone } from '$lib/timezone';
 import { childState } from '$lib/collections/childProgress';
 import { commitmentAgendaEntry, commitmentDisplayState, describeRecurrence, type ItemCommitmentSnapshot } from '$lib/commitment';
 import { acknowledgedText, dateTime, deadlinePill, entryKey, forecastText, isDateOnly, roleText, snoozeLabel } from '$lib/calendar/presentation';
 import StateMark from '$lib/components/calendar/StateMark.svelte';
 import OccurrenceActions from '$lib/components/calendar/OccurrenceActions.svelte';
 import SnoozeLabel from '$lib/components/calendar/SnoozeLabel.svelte';
 import ResolveOccurrenceDialog from '$lib/components/calendar/ResolveOccurrenceDialog.svelte';
 import ScheduleEndDialog from '$lib/components/calendar/ScheduleEndDialog.svelte';
 import MoveImpactDialog from '$lib/components/calendar/MoveImpactDialog.svelte';
 import ForecastDialog from '$lib/components/calendar/ForecastDialog.svelte';
 import ItemCommitmentDateDialog from './ItemCommitmentDateDialog.svelte';
 import { createItemCommitmentActions, permissionCopy } from './itemCommitmentActions.svelte';

 interface Props {
  wsSlug: string;
  itemSlug: string;
  item: Item;
  collection: Collection | null;
  onSnapshot: (snapshot: ItemCommitmentSnapshot) => void;
  /** Owned by the item header's "+ Add date" button; the panel owns the dialog and the write. */
  addDateOpen?: boolean;
  onAddDateClose?: () => void;
  /** The host is view-only (a peeking master): dates show, no write is offered. */
  readOnly?: boolean;
 }
 let { wsSlug, itemSlug, item, collection, onSnapshot, addDateOpen = false, onAddDateClose, readOnly = false }: Props = $props();
 const itemId = $derived(item.id);
 const zone = $derived(authStore.user?.timezone || browserZone() || 'UTC');
 let snapshot = $state.raw<ItemCommitmentSnapshot | null>(null);
 let failed = $state(false);
 let now = $state(new Date(Date.now()));
 let generation = 0;
 let disposed = false;
 let refreshQueued = false;
 let requestedIdentity = '';
 let addSerial = 0;
 let mobile = $state(false);
 let added = $state('');
 let section = $state<HTMLElement>();
 const actions = createItemCommitmentActions({
  identity: () => ({ wsSlug, itemSlug, itemId: item.id }),
  zone: () => zone,
  refresh: () => load(),
  root: () => section
 });

 function current(s: ItemCommitmentSnapshot): boolean {
  return !disposed && s.wsSlug === wsSlug && s.itemId === item.id && s.itemSlug === itemSlug
   && s.authEpoch === authStore.identityEpoch && s.zone === zone;
 }
 const view = $derived(snapshot && current(snapshot) ? snapshot.view : undefined);
 const unavailable = $derived(!!snapshot && current(snapshot) && failed);
 const itemClosed = $derived(childState(item, collection ?? undefined) !== 'open');
 const display = $derived(view ? commitmentDisplayState({ ...view, itemClosed }, now) : null);
 // The server's display hint; mutations re-check it. Absent or stale means no writes offered.
 const editable = $derived(!readOnly && !!snapshot && current(snapshot) && snapshot.editable);
 const entry = $derived(view && editable && !itemClosed ? commitmentAgendaEntry(view, { wsSlug, item, collection, zone }) : null);
 const dialogOpen = $derived(!!(actions.dialogEntry || actions.review || actions.endChoice || actions.forecastEntry || addDateOpen));

 async function load() {
  const ticket = ++generation;
  const isSameIdentity = authStore.identityFence();
  const target: ItemCommitmentSnapshot = { wsSlug, itemId: item.id, itemSlug, authEpoch: authStore.identityEpoch, zone, view: undefined, editable: false };
  failed = false;
  // A background refresh of the same identity keeps the shown view until its
  // answer arrives, so an edit to the item doesn't blank the section.
  if (!(snapshot && current(snapshot) && snapshot.view !== undefined)) {
   snapshot = target;
   onSnapshot(target);
  }
  const fresh = () => isSameIdentity() && ticket === generation && current(target);
  try {
   const loaded = await api.getCommitment(target.wsSlug, target.itemSlug);
   if (!fresh()) return true;
   snapshot = { ...target, view: loaded, editable: loaded.editable === true };
   actions.refreshed();
   onSnapshot(snapshot);
   return true;
  } catch (error) {
   if (!fresh()) return true;
   // The client preserves the server's not_found code (HTTP 404), not HTTP status.
   if ((error as { code?: string })?.code !== 'not_found') { failed = true; return false; }
  }
  // No commitment: the gates read confirms the item is still readable and says
  // whether this caller may add a date. Its failure is unavailable, not "no date".
  try {
   const gates = await api.getCommitmentGates(target.wsSlug, target.itemSlug);
   if (!fresh()) return true;
   snapshot = { ...target, view: null, gates, editable: gates.editable === true };
   actions.refreshed();
   onSnapshot(snapshot);
   return true;
  } catch {
   if (!fresh()) return true;
   failed = true;
   return false;
  }
 }
 async function created() {
  onAddDateClose?.();
  added = '';
  const sameIdentity = authStore.identityFence();
  const announced = ++addSerial;
  await load();
  if (sameIdentity() && announced === addSerial && !disposed) added = 'Date added.';
 }

 $effect(() => {
  const identity = JSON.stringify([wsSlug, itemId, itemSlug, authStore.identityEpoch, zone]);
  if (identity === requestedIdentity) return;
  requestedIdentity = identity;
  untrack(() => { actions.reset(); if (addDateOpen) onAddDateClose?.(); added = ''; addSerial++; void load(); });
 });
 onMount(() => {
  const query = window.matchMedia('(max-width: 768px)');
  mobile = query.matches;
  const update = (event: MediaQueryListEvent) => { mobile = event.matches; };
  query.addEventListener('change', update);
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
  return () => { query.removeEventListener('change', update); unsubscribe(); clearInterval(timer); };
 });
 onDestroy(() => { disposed = true; generation++; actions.dispose(); });
</script>

<!-- No commitment renders no section (no layout shift once the read lands);
     "+ Add date" lives in the item header. The live region stays mounted,
     visually hidden, so "Date added." is announced before the re-read lands. --><p class="sr-only" role="status" aria-live="polite" aria-atomic="true">{actions.announcement || added}</p>{#if view || unavailable}
 <section class="commitment-panel" aria-label="When" bind:this={section} tabindex="-1">
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
   {#if entry}
    <div class="occurrence-actions">
     <SnoozeLabel label={snoozeLabel(entry, zone, now)} />
     <OccurrenceActions {entry} {zone} pending={!!actions.pending[entryKey(entry)]} disabled={!!actions.saved[entryKey(entry)]} onaction={actions.action} />
    </div>
   {/if}
   {#each display.annotations as annotation}<p>{annotation}</p>{/each}
   {#if display.acknowledgedAt || display.acknowledgedBy}<p>{acknowledgedText(display.acknowledgedAt, display.acknowledgedBy, zone, now)}</p>{/if}
   {#if display.unavailable}<button onclick={() => load()}>Retry</button>
   {:else if actions.refreshFailed}<p role="status" class="refresh-failed">Saved. Could not refresh. <button onclick={() => actions.retryRefresh()}>Retry</button></p>
   {:else if unavailable}<p role="status" class="refresh-failed">Couldn't refresh these dates. <button onclick={() => load()}>Retry</button></p>{/if}
  {:else}
   <p role="status">Date unavailable</p><button onclick={() => load()}>Retry</button>
  {/if}
  {#if actions.actionError && !dialogOpen}<p role="alert">{actions.actionError}</p>{/if}
 </section>
{/if}{#if editable}
 <ItemCommitmentDateDialog open={addDateOpen} {wsSlug} {itemSlug} itemRef={formatItemRef(item) ?? ''} itemTitle={item.title} {mobile} onclose={() => onAddDateClose?.()} oncreated={() => void created()} />
 <ForecastDialog open={!!actions.forecastEntry} entry={actions.forecastEntry} {zone} {mobile} {permissionCopy} onclose={actions.closeForecast} onsaved={actions.forecastSaved} />
 <ResolveOccurrenceDialog open={!!actions.dialogEntry} mode={actions.dialogMode} entry={actions.dialogEntry} {zone} {mobile}
  pending={actions.dialogEntry ? !!actions.pending[entryKey(actions.dialogEntry)] : false} error={actions.actionError}
  onclose={actions.closeDialog} onsubmit={actions.submitDialog} />
 <ScheduleEndDialog open={!!actions.endChoice} entry={actions.endChoice?.entry ?? null} details={actions.endChoice?.details ?? null} {mobile}
  pending={actions.endChoice ? !!actions.pending[entryKey(actions.endChoice.entry)] : false} error={actions.endError}
  onclose={actions.closeEnd} onchoose={choice => void actions.chooseEnd(choice)} />
 {#if actions.review}
  <MoveImpactDialog entry={actions.review.entry} preview={actions.review.preview} {zone} {mobile} titles={actions.linkedTitles} initialReason={actions.review.reason}
   pending={actions.committing} error={actions.actionError} onclose={actions.cancelReview}
   onsubmit={(reason, acknowledge, confirmPastDeadline) => void actions.commitReview(reason, acknowledge, confirmPastDeadline)} />
 {/if}
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
 .commitment-panel:focus { outline: none; }
 .occurrence-actions { display: flex; flex-wrap: wrap; align-items: center; gap: var(--space-2); margin: var(--space-2) 0; }
 .sr-only { position: absolute; width: 1px; height: 1px; padding: 0; margin: -1px; overflow: hidden; clip: rect(0, 0, 0, 0); white-space: nowrap; border: 0; }
 summary { align-content: center; }
 button { color: var(--text-primary); background: var(--bg-secondary); border: 1px solid var(--border); border-radius: var(--radius-sm); padding: var(--space-2) var(--space-3); }
 @container (max-width: 360px) { .date-row { flex-direction: column; gap: var(--space-1); } .label { width: auto; } }
 @media (max-width: 480px) { .date-row { flex-direction: column; gap: var(--space-1); } .label { width: auto; } }
</style>
