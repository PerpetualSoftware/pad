<script lang="ts">
 import { tick, untrack } from 'svelte';
 import { api } from '$lib/api/client';
 import type { CalendarResponse, Collection, Item } from '$lib/types';
 import { authStore } from '$lib/stores/auth.svelte';
 import { viewport } from '$lib/stores/breakpoint.svelte';
 import { attentionFirst, calendarError, calendarRows, dayLabel, fieldLabel, incomplete, incompleteDay, monthLabel, monthWindow, moveMonth, safeZone, staleConfig, STALE_FIELD, UNSUPPORTED_FIELD, zoneDay, type CalendarConfig, type CalendarRow } from '$lib/calendar/monthGrid';
 import MonthGrid from '../calendar/MonthGrid.svelte';
 import CalendarToolbar from '../calendar/CalendarToolbar.svelte';
 import CalendarEntry from '../calendar/CalendarEntry.svelte';
 import CalendarNotice from '../calendar/CalendarNotice.svelte';
 import CalendarDayDialog from '../calendar/CalendarDayDialog.svelte';
 import CalendarFieldPicker from '../calendar/CalendarFieldPicker.svelte';
 import ContentSkeleton from '../common/ContentSkeleton.svelte';
 import Button from '../common/Button.svelte';
 import Modal from '../common/Modal.svelte';
 import BottomSheet from '../common/BottomSheet.svelte';

 let {collection, wsSlug, basePath, config, onConfigChange, filters = {}, filterProblem = '', onItemOpen, onItemRefOpen, onChooseFields, onFilters = () => {}, onList = () => {}, onClearFilters = () => {}, refreshKey = 0, pickerOpen = $bindable(false), initialMonth}: {
  collection:Collection;wsSlug:string;basePath:string;config:CalendarConfig;onConfigChange:(config:CalendarConfig)=>void;
  filters?:Record<string,string>;filterProblem?:string;onChooseFields?:()=>void;onItemOpen?:(item:Item)=>void;onItemRefOpen?:(ref:string)=>void;
  onFilters?:()=>void;onList?:()=>void;onClearFilters?:()=>void;refreshKey?:number;pickerOpen?:boolean;initialMonth?:string;
 } = $props();
 let zone = $derived(safeZone(authStore.user?.timezone));
 // Filters settle before a request is made, so typing does not fire one per key.
 // A change of workspace, collection or schema adopts the incoming filters at once:
 // the previous collection's settled filters never meet the new collection.
 const FILTER_SETTLE_MS = 250;
 let applied = $state(untrack(() => ({filters, filterProblem})));
 let settleTimer: ReturnType<typeof setTimeout> | undefined;
 let appliedScope = untrack(() => JSON.stringify([wsSlug, collection.id, collection.schema]));
 $effect(() => {
  const scope = JSON.stringify([wsSlug, collection.id, collection.schema]);
  const next = {filters, filterProblem};
  const key = JSON.stringify(next);
  clearTimeout(settleTimer);
  if (scope !== appliedScope) { appliedScope = scope; applied = next; return; }
  if (key === untrack(() => JSON.stringify(applied))) return;
  settleTimer = setTimeout(() => { applied = next; }, FILTER_SETTLE_MS);
  return () => clearTimeout(settleTimer);
 });
 let zoneSet = $derived(!!authStore.user?.timezone && zone === authStore.user.timezone);
 let today = $derived(zoneDay(new Date(),zone));
 let month = $state(untrack(() => initialMonth ?? zoneDay(new Date(), zone).slice(0,7)));
 let selected = $state('');
 let openedDay = $state<string|null>(null);
 let data = $state<CalendarResponse|null>(null);
 let error = $state('');
 let loading = $state(false);
 let retry = $state(0);
 let ticket = 0;
 let loadedScope = '';
 let loadedBase = '';
 let lastUnrelated = '';
 let grid = $state<{ cancelFocus:()=>void; focusDate:(day:string)=>void }>();
 let rows = $derived(data ? calendarRows(data,collection,config.date_field ?? '',month) : []);
 let stale = $derived(staleConfig(collection,config) || error === STALE_FIELD || error === UNSUPPORTED_FIELD);
 let focusDay = $derived(selected.startsWith(month) ? selected : today.startsWith(month) ? today : `${month}-01`);
 let byDay = $derived.by(() => {
  const map = new Map<string,CalendarRow[]>();
  for (const row of rows) { const list = map.get(row.day) ?? []; list.push(row); map.set(row.day,list); }
  // What needs attention leads its day, so a missed deadline is never behind "+N more".
  for (const [day, list] of map) map.set(day, attentionFirst(list, row => !!row.attention));
  return map;
 });

 $effect(() => {
  const {filters: appliedFilters, filterProblem: appliedProblem} = applied;
  const unrelated = JSON.stringify([wsSlug,collection.id,collection.schema,config,appliedFilters,appliedProblem,zone,authStore.identityEpoch]);
  // Month, collection, field and zone invalidate the grid; a filter change keeps it until the new result lands.
  const base = JSON.stringify([wsSlug,collection.id,collection.schema,config,month,zone,authStore.identityEpoch]);
  const scope = JSON.stringify([base,appliedFilters,appliedProblem]);
  refreshKey; retry;
  const query = {...appliedFilters,...config,date_field:config.date_field ?? '',...monthWindow(month,zone),tz:zone};
  const invalid = staleConfig(collection,config) || !config.date_field || !!appliedProblem;
  const request = ++ticket;
  const held = authStore.identityFence();
  const ws = wsSlug, coll = collection.slug;
  untrack(() => {
   const same = scope === loadedScope;
   const sameBase = base === loadedBase;
   // A pending keyboard focus belongs to a month change only.
   if (unrelated !== lastUnrelated) grid?.cancelFocus();
   lastUnrelated = unrelated;
   if (!sameBase || invalid) { data = null; openedDay = null; }
   error = ''; loading = !invalid;
   if (invalid) { grid?.cancelFocus(); return; }
   api.calendar.get(ws,coll,query).then(result => {
    if (request !== ticket || !held()) return;
    data = result; loadedScope = scope; loadedBase = base; loading = false;
   }).catch(reason => {
    if (request !== ticket || !held()) return;
    error = calendarError(reason); loading = false; grid?.cancelFocus();
    if (error === STALE_FIELD || error === UNSUPPORTED_FIELD) data = null;
    else if (!same) data = null;
    else if (data && error === 'Could not load calendar.') error = 'Could not update calendar. Showing the previous result.';
   });
  });
  return () => { ticket++; };
 });
 function chooseDay(day: string, focus = false) {
  selected = day;
  month = day.slice(0,7);
  if (focus) grid?.focusDate(day);
 }
 function pageMonth(delta: number) { chooseDay(moveMonth(focusDay,delta)); }
 function openDay(day:string) { selected=day; openedDay=day; }
 async function openEntry(row:CalendarRow) {
  openedDay=null;
  await tick();
  if(row.item && onItemOpen) onItemOpen(row.item);
  else if(onItemRefOpen) onItemRefOpen(row.target);
  else window.location.assign(`${basePath}/${encodeURIComponent(row.target)}`);
 }
 function changeConfig(next:CalendarConfig) { onConfigChange(next); }
 function filtersAction() { openedDay=null; onFilters(); }
 function listAction() { openedDay=null; onList(); }
 function cellLabel(day:string) { return `${dayLabel(day,true)}${day===today ? ', Today' : ''}, ${byDay.get(day)?.length ?? 0} entries${data && incompleteDay(data,day) ? ', Incomplete' : ''}`; }
</script>

<section class="calendar" class:updating={loading && !!data} aria-label="Calendar" aria-busy={loading && !!data}>
 <CalendarToolbar {month} {zone} zoneSet={zoneSet} dateLabel={fieldLabel(collection,config.date_field)} onprevious={() => pageMonth(-1)} onnext={() => pageMonth(1)} ontoday={() => chooseDay(today,true)} onfields={() => { if (onChooseFields) onChooseFields(); else pickerOpen=true; }} />
 <p class="sr-only" role="status">{monthLabel(month)}{loading ? data ? ' — Updating calendar…' : ' — Loading calendar…' : ''}</p>
 {#if stale || !config.date_field}
  <p>{error === UNSUPPORTED_FIELD && !staleConfig(collection,config) ? UNSUPPORTED_FIELD : stale ? STALE_FIELD : 'Choose a date field'}</p>
  <CalendarFieldPicker {collection} {config} onchange={changeConfig} />
 {:else if applied.filterProblem}
  <p role="alert">{applied.filterProblem}</p><Button onclick={onFilters}>Filters</Button><Button onclick={onList}>Use list</Button>
 {:else}
  {#if error}<div role="alert"><p>{error}</p>{#if error.includes('time zone')}<a href="/console/settings">Time zone settings</a>{:else}<Button onclick={() => retry++}>Try again</Button>{/if}</div>{/if}
  {#if loading && !data}<ContentSkeleton />{/if}
  {#if data}
   <CalendarNotice {data} onfilters={onFilters} onlist={onList} onretry={() => retry++} />
   {#if !rows.length && !incomplete(data)}
    {#if Object.keys(applied.filters).length}<p>No entries match these filters.</p><Button onclick={onClearFilters}>Clear filters</Button>
    {:else}<p>No entries this month.</p><p>Choose another month or date field.</p>{/if}
   {/if}
  {/if}
  <MonthGrid bind:this={grid} {month} {today} {selected} mobile={viewport.isMobile} {cellLabel}
   summary={(day) => data && incompleteDay(data,day) ? 'Not fully loaded' : byDay.get(day)?.length ? `${byDay.get(day)!.length} entries` : 'No entries'}
   count={(day) => byDay.get(day)?.length ? String(byDay.get(day)!.length) : ''} empty={(day) => !byDay.get(day)?.length && !(data && incompleteDay(data,day))}
   onopen={openDay} onchoose={chooseDay} loaded={data} cell={entries} />
  {#if data}
   <CalendarDayDialog day={openedDay} rows={openedDay ? byDay.get(openedDay) ?? [] : []} {data} {zone} {basePath} onclose={() => openedDay=null} onopen={openEntry} onfilters={filtersAction} onlist={listAction} />
  {/if}
 {/if}
</section>
{#snippet entries(day:string, mobile:boolean)}
 {#each (byDay.get(day) ?? []).slice(0,3) as row (row.key)}<CalendarEntry {row} {zone} {basePath} onopen={openEntry} preview keyboard={mobile} />{/each}
 {#if (byDay.get(day)?.length ?? 0)>3}<button class="more" tabindex={mobile ? 0 : -1} onclick={() => openDay(day)}>+{byDay.get(day)!.length-3} more</button>{/if}
 {#if data}
  {#if incompleteDay(data,day)}<p class="incomplete-day">Not fully loaded</p>{/if}
  {#if data.partial_day===day}<CalendarNotice {data} {day} keyboard={mobile} onfilters={onFilters} onlist={onList} />{/if}
 {/if}
{/snippet}
{#if viewport.isMobile}
 {#if pickerOpen}<BottomSheet open onclose={() => pickerOpen=false} title="Choose a date field"><CalendarFieldPicker {collection} {config} onchange={changeConfig} onclose={() => pickerOpen=false} /></BottomSheet>{/if}
{:else}
 <Modal open={pickerOpen} onclose={() => pickerOpen=false} ariaLabel="Choose a date field"><CalendarFieldPicker {collection} {config} onchange={changeConfig} onclose={() => pickerOpen=false} /></Modal>
{/if}
<style>
 .calendar { min-width:0; padding:var(--space-4); }
 .updating { opacity:0.6; }
 button { font:inherit; color:var(--text-primary); border:0; background:transparent; cursor:pointer; border-radius:var(--radius-sm); }
 button:hover { background:var(--bg-hover); }
 button:focus-visible { outline:2px solid var(--accent-primary); outline-offset:-2px; }
 .more { text-align:left; font-size:11.5px; font-weight:600; color:var(--text-secondary); padding:1px 4px; }
 .incomplete-day { color:var(--text-secondary); font-size:11px; margin:0; padding:0 4px; }
 .sr-only { position:absolute; width:1px; height:1px; overflow:hidden; clip-path:inset(50%); }
 @media (pointer:coarse) { button, .calendar :global(.btn) { min-height:44px; min-width:44px; } }
 @media (max-width:768px) { .calendar { padding:var(--space-2); } }
</style>
