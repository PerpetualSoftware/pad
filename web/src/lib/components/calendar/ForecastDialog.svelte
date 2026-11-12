<script lang="ts">
 import { untrack } from 'svelte';
 import { api } from '$lib/api/client';
 import Button from '$lib/components/common/Button.svelte';
 import DialogShell from '$lib/components/common/DialogShell.svelte';
 import { forecastPill, forecastSetter, isDateOnly, localTime, roleDay, shortRoleDate, wallToInstant } from '$lib/calendar/presentation';
 import type { AgendaEntry, ItemCommitmentView } from '$lib/types';

 let {open, entry, zone, mobile, permissionCopy = 'You can no longer change this item.', onclose, onsaved}: {
  open: boolean; entry: AgendaEntry | null; zone: string; mobile: boolean; permissionCopy?: string;
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
 const current = $derived(view?.forecast);
 // Late by the pill's own rule, so the dialog and the row never disagree.
 const deadline = $derived(current && view?.roles?.deadline && forecastPill(current.value, view.roles.deadline, zone).late ? view.roles.deadline : null);
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

 <DialogShell {open} {mobile} {pending} title="Forecast" subject={[entry?.item_title, entry?.workspace_name].filter(Boolean).join(' · ')} subjectRef={entry?.item_ref} {onclose}>
 <form id={`${id}-form`} onsubmit={(event) => {event.preventDefault(); void save();}}>
  {#if loading}<p class="quiet">Loading forecast…</p>{/if}
  {#if current}
   <div class="current" data-role="forecast">
    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 9c3-3 5 3 8 0s5 3 8 0" /><path d="M4 15c3-3 5 3 8 0s5 3 8 0" /></svg>
    <div>
     <div class="value">{shortRoleDate(current.value, zone)}</div>
     <div class="by">Set by {forecastSetter(current)}{#if Number.isFinite(Date.parse(current.set_at))}, {shortRoleDate(current.set_at, zone)}{/if}{#if current.source}{' '}· “{current.source}”{/if}</div>
     {#if current.passed}<div class="by">This date has passed.</div>{/if}
    </div>
   </div>
  {/if}
  {#if deadline}
   <div class="callout">
    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 4l9 16H3L12 4z" /><path d="M12 10v4M12 17v.5" /></svg>
    <span>This forecast is after the hard deadline ({shortRoleDate(deadline, zone)}).</span>
   </div>
  {/if}
  {#if summary && summary.total > 0}
   <div class="children">
    <p>{summary.counted} of {summary.total} counted · {summary.waivers} waivers · {resultText}</p>
    <ul>{#each summary.children as child, index (index)}
     <li>{child.item_ref} · {child.close_state}{#if child.waived} · Waiver: {child.reason}{/if}</li>
    {/each}</ul>
   </div>
  {/if}
  <div class="two">
   <div class="field">
    <label for={`${id}-date`}>New forecast date</label>
    <input id={`${id}-date`} type="date" required bind:value={date} disabled={loading || pending || !view} />
   </div>
   <div class="field">
    <label for={`${id}-time`}>Time <span class="optional">(optional)</span></label>
    <input id={`${id}-time`} type="time" bind:value={time} disabled={loading || pending || !view} />
    <p class="hint">{zone}</p>
   </div>
  </div>
  <div class="field">
   <label for={`${id}-source`}>Where does this estimate come from? <span class="optional">(optional)</span></label>
   <textarea id={`${id}-source`} maxlength="280" bind:value={source} disabled={loading || pending || !view}></textarea>
  </div>
  {#if error}<p role="alert">{error}</p>{/if}
 </form>
 {#snippet footerStart()}
  {#if current}<Button variant="ghost" disabled={loading || pending} onclick={() => void save(true)}>Clear forecast</Button>{/if}
 {/snippet}
 {#snippet footer()}
  <Button disabled={pending} onclick={() => { if (!pending) onclose(); }}>Cancel</Button>
  <Button variant="primary" type="submit" form={`${id}-form`} disabled={loading || pending || !view}>Save forecast</Button>
 {/snippet}
</DialogShell>

<style>
 form { display: flex; flex-direction: column; gap: var(--space-4); min-width: 0; }
 p { margin: 0; }
 .quiet, .hint, .by { color: var(--text-muted); font-size: 12px; }
 .hint { margin-top: 6px; overflow-wrap: anywhere; }
 .current { display: flex; align-items: center; gap: var(--space-3); padding: 10px 12px; border: 1px dashed var(--border-strong); border-radius: var(--radius); }
 .current svg { flex: 0 0 auto; color: var(--text-secondary); }
 .value { font-weight: 700; }
 .callout { display: flex; gap: var(--space-3); padding: 10px 12px; border: 1px solid color-mix(in srgb, var(--accent-red) 35%, transparent); border-radius: var(--radius); background: color-mix(in srgb, var(--accent-red) 10%, transparent); font-size: 13px; }
 .callout svg { flex: 0 0 auto; margin-top: 2px; color: var(--accent-red); }
 .children { font-size: 13px; color: var(--text-secondary); }
 .children ul { margin: var(--space-1) 0 0; padding-left: var(--space-5); }
 .two { display: grid; grid-template-columns: 1fr 1fr; gap: var(--space-3); }
 .field { min-width: 0; }
 label { display: block; margin-bottom: 6px; font-size: 12px; font-weight: 600; color: var(--text-secondary); }
 .optional { font-weight: 400; color: var(--text-muted); }
 input, textarea { box-sizing: border-box; width: 100%; min-width: 0; min-height: 36px; padding: 6px 10px; color: var(--text-primary); background: var(--bg-primary); border: 1px solid var(--border); border-radius: var(--radius); font: inherit; font-size: 14px; }
 textarea { display: block; min-height: 72px; resize: vertical; }
 @media (max-width: 768px) { .two { grid-template-columns: 1fr; } input { min-height: 44px; } }
</style>
