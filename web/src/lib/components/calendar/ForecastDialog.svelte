<script lang="ts">
 import { untrack } from 'svelte';
 import { api } from '$lib/api/client';
 import Button from '$lib/components/common/Button.svelte';
 import Modal from '$lib/components/common/Modal.svelte';
 import { forecastText, isDateOnly, localTime, roleDay, wallToInstant } from '$lib/calendar/presentation';
 import type { AgendaEntry, ItemCommitmentView } from '$lib/types';

 let {open, entry, zone, permissionCopy = 'You can no longer change this item.', onclose, onsaved}: {
  open: boolean; entry: AgendaEntry | null; zone: string; permissionCopy?: string;
  onclose: () => void; onsaved: () => void;
 } = $props();
 const id = $props.id();
 let view = $state<ItemCommitmentView | null>(null);
 let date = $state('');
 let time = $state('');
 let source = $state('');
 let loading = $state(false);
 let pending = $state(false);
 let error = $state('');
 let ticket = 0;
 const summary = $derived(view?.milestone);
 const resultText = $derived(summary?.result === 'completed' ? 'Completed' : summary?.result === 'pending' ? 'Pending' : summary?.result === 'unknown' ? 'Unknown: includes items you cannot see' : 'No children selected');

 function errorCopy(e: unknown) {
  const err = e as {status?: number; code?: string; message?: string};
  return err.status === 403 || err.code === 'forbidden' || err.code === 'permission_denied' ? permissionCopy : err.message || 'Could not update the forecast. Try again.';
 }
 async function load(row: AgendaEntry, timeZone: string, mine: number) {
  loading = true; pending = false; error = ''; view = null; date = ''; time = ''; source = '';
  try {
   const next = await api.getCommitment(row.workspace!, row.item_ref);
   if (mine !== ticket) return;
   view = next;
   if (next.forecast) {
    date = roleDay(next.forecast.value, timeZone);
    time = isDateOnly(next.forecast.value) ? '' : localTime(next.forecast.value, timeZone);
    source = next.forecast.source;
   }
  } catch (e) { if (mine === ticket) error = errorCopy(e); }
  finally { if (mine === ticket) loading = false; }
 }
 $effect(() => {
  const visible = open, row = entry, timeZone = zone;
  untrack(() => {
   const mine = ++ticket;
   if (visible && row) void load(row, timeZone, mine);
  });
 });
 async function save(clear = false) {
  if (pending || loading || !entry || !view) return;
  if (!clear && !date) {error = 'Choose a date.'; return;}
  const mine = ticket, row = entry;
  pending = true; error = '';
  try {
   if (clear) await api.clearCommitmentForecast(row.workspace!, row.item_ref);
   else {
    const value = time ? wallToInstant(`${date}T${time}`, zone) : date;
    await api.setCommitmentForecast(row.workspace!, row.item_ref, {value, ...(source ? {source} : {})});
   }
   if (mine === ticket) onsaved();
  } catch (e) { if (mine === ticket) error = errorCopy(e); }
  finally { if (mine === ticket) pending = false; }
 }
</script>

<Modal {open} labelledby={`${id}-heading`} onclose={() => {if (!pending) onclose();}}>
 <form onsubmit={(event) => {event.preventDefault(); void save();}}>
  <h2 id={`${id}-heading`}>Forecast</h2>
  {#if loading}<p>Loading forecast…</p>{/if}
  {#if view?.forecast}
   <div class="forecast" data-role="forecast">{forecastText(view.forecast, zone)}</div>
   <p>Set at {view.forecast.set_at}</p>
   {#if view.forecast.source}<p>Source: {view.forecast.source}</p>{/if}
   {#if view.forecast.passed}<p>This date has passed.</p>{/if}
  {/if}
  {#if summary}
   <p>{summary.counted} of {summary.total} counted · {summary.waivers} waivers · {resultText}</p>
   <ul>{#each summary.children as child, index (index)}
    <li>{child.item_ref} · {child.close_state}{#if child.waived} · Waiver: {child.reason}{/if}</li>
   {/each}</ul>
  {/if}
  <label for={`${id}-date`}>Date</label>
  <input id={`${id}-date`} type="date" required bind:value={date} disabled={loading || pending || !view} />
  <label for={`${id}-time`}>Time (optional)</label>
  <input id={`${id}-time`} type="time" bind:value={time} disabled={loading || pending || !view} />
  <p>{zone}</p>
  <label for={`${id}-source`}>Source (optional)</label>
  <textarea id={`${id}-source`} maxlength="280" bind:value={source} disabled={loading || pending || !view}></textarea>
  {#if error}<p role="alert">{error}</p>{/if}
  <div class="footer">
   <Button disabled={pending} onclick={onclose}>Cancel</Button>
   {#if view?.forecast}<Button disabled={loading || pending} onclick={() => void save(true)}>Clear forecast</Button>{/if}
   <Button type="submit" disabled={loading || pending || !view}>Save</Button>
  </div>
 </form>
</Modal>

<style>
 form { padding: var(--space-5); overflow-y: auto; min-width: 0; }
 h2 { font-size: 1.2rem; margin: 0 0 var(--space-4); }
 label { display: block; margin: var(--space-3) 0 var(--space-2); font-weight: 600; }
 input, textarea { box-sizing: border-box; min-width: 0; max-width: 100%; padding: var(--space-2); color: var(--text-primary); background: var(--bg-primary); border: 1px solid var(--border); border-radius: var(--radius); font: inherit; }
 textarea { display: block; width: 100%; min-height: 5rem; resize: vertical; }
 .forecast { border: 1px dashed var(--border-strong); padding: var(--space-2); }
 .footer { display: flex; justify-content: flex-end; flex-wrap: wrap; gap: var(--space-2); margin-top: var(--space-5); }
</style>
