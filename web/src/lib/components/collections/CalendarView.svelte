<script lang="ts">
 import { tick, untrack } from 'svelte';
 import { api } from '$lib/api/client';
 import type { CalendarResponse, Collection, Item } from '$lib/types';
 import { authStore } from '$lib/stores/auth.svelte';
 import { viewport } from '$lib/stores/breakpoint.svelte';
 import { calendarError, calendarRows, dayLabel, fieldLabel, incomplete, incompleteDay, monthDays, monthLabel, monthWindow, moveMonth, shiftDay, staleConfig, STALE_FIELD, zoneDay, type CalendarConfig, type CalendarRow } from '$lib/calendar/presentation';
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
 const uid = $props.id();
 let zone = $derived(authStore.user?.timezone || 'UTC');
 let today = $derived(zoneDay(new Date(),zone));
 let month = $state(untrack(() => initialMonth ?? zoneDay(new Date(), zone).slice(0,7)));
 let selected = $state('');
 let openedDay = $state<string|null>(null);
 let data = $state<CalendarResponse|null>(null);
 let error = $state('');
 let loading = $state(false);
 let retry = $state(0);
 let root: HTMLElement;
 let ticket = 0;
 let loadedScope = '';
 let pendingFocus: string | null = null;
 let days = $derived(monthDays(month));
 let rows = $derived(data ? calendarRows(data,collection,config.date_field ?? '',month) : []);
 let stale = $derived(staleConfig(collection,config) || error === STALE_FIELD);
 let focusDay = $derived(selected.startsWith(month) ? selected : today.startsWith(month) ? today : `${month}-01`);
 let leading = $derived((new Date(`${month}-01T12:00:00Z`).getUTCDay() + 6) % 7);
 let cells = $derived([...Array<string|null>(leading).fill(null), ...days, ...Array<string|null>((7 - (leading + days.length) % 7) % 7).fill(null)]);
 let weeks = $derived(Array.from({length:cells.length/7}, (_, i) => cells.slice(i*7,i*7+7)));
 let byDay = $derived.by(() => {
  const map = new Map<string,CalendarRow[]>();
  for (const row of rows) { const list = map.get(row.day) ?? []; list.push(row); map.set(row.day,list); }
  return map;
 });

 $effect(() => {
  const scope = JSON.stringify([wsSlug,collection.id,collection.schema,config,filters,filterProblem,month,zone,authStore.identityEpoch]);
  refreshKey; retry;
  const query = {...filters,...config,date_field:config.date_field ?? '',...monthWindow(month,zone),tz:zone};
  const invalid = staleConfig(collection,config) || !config.date_field || !!filterProblem;
  const request = ++ticket;
  const held = authStore.identityFence();
  const ws = wsSlug, coll = collection.slug;
  untrack(() => {
   const same = scope === loadedScope;
   if (!same || invalid) { data = null; openedDay = null; }
   error = ''; loading = !invalid;
   if (invalid) return;
   api.calendar.get(ws,coll,query).then(result => {
    if (request !== ticket || !held()) return;
    data = result; loadedScope = scope; loading = false;
   }).catch(reason => {
    if (request !== ticket || !held()) return;
    error = calendarError(reason); loading = false;
    if (error === STALE_FIELD) data = null;
    else if (same && data && error === 'Could not load calendar.') error = 'Could not update calendar. Showing the previous result.';
   });
  });
  return () => { ticket++; };
 });
 $effect(() => {
  data; viewport.isMobile;
  if (pendingFocus) { const day = pendingFocus; void tick().then(() => { const el = root?.querySelector<HTMLButtonElement>(`[data-day="${day}"]`); if (el) { el.focus(); pendingFocus = null; } }); }
 });
 $effect.pre(() => {
  viewport.isMobile;
  if (root?.contains(document.activeElement) && document.activeElement instanceof HTMLElement && document.activeElement.dataset.day) pendingFocus = document.activeElement.dataset.day;
 });
 function chooseDay(day: string, focus = false) {
  selected = day;
  month = day.slice(0,7);
  if (focus) { pendingFocus = day; void tick().then(() => root?.querySelector<HTMLButtonElement>(`[data-day="${day}"]`)?.focus()); }
 }
 function pageMonth(delta: number) { chooseDay(moveMonth(focusDay,delta)); }
 function keydown(event: KeyboardEvent, day: string) {
  const weekday = (new Date(`${day}T12:00:00Z`).getUTCDay()+6)%7;
  let target: string;
  switch(event.key) {
   case 'ArrowLeft': target=shiftDay(day,-1); break;
   case 'ArrowRight': target=shiftDay(day,1); break;
   case 'ArrowUp': target=shiftDay(day,-7); break;
   case 'ArrowDown': target=shiftDay(day,7); break;
   case 'Home': target=shiftDay(day,-weekday); if(target.slice(0,7)!==month) target=days[0]; break;
   case 'End': target=shiftDay(day,6-weekday); if(target.slice(0,7)!==month) target=days[days.length-1]; break;
   case 'PageUp': target=moveMonth(day,-1); break;
   case 'PageDown': target=moveMonth(day,1); break;
   default: return;
  }
  event.preventDefault(); event.stopPropagation(); chooseDay(target,true);
 }
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

<section class="calendar" bind:this={root} aria-label="Calendar">
 <CalendarToolbar {month} {zone} zoneSet={!!authStore.user?.timezone} dateLabel={fieldLabel(collection,config.date_field)} onprevious={() => pageMonth(-1)} onnext={() => pageMonth(1)} ontoday={() => chooseDay(today,true)} onfields={() => { if (onChooseFields) onChooseFields(); else pickerOpen=true; }} />
 <p class="sr-only" role="status">{monthLabel(month)}{loading ? data ? ' — Updating calendar…' : ' — Loading calendar…' : ''}</p>
 {#if stale || !config.date_field}
  <p>{stale ? STALE_FIELD : 'Choose a date field'}</p>
  <CalendarFieldPicker {collection} {config} onchange={changeConfig} />
 {:else if filterProblem}
  <p role="alert">{filterProblem}</p><Button onclick={onFilters}>Filters</Button><Button onclick={onList}>Use list</Button>
 {:else}
  {#if error}<div role="alert"><p>{error}</p>{#if error.includes('time zone')}<a href="/console/settings">Time zone settings</a>{:else}<Button onclick={() => retry++}>Try again</Button>{/if}</div>{/if}
  {#if loading && !data}<ContentSkeleton />{/if}
  {#if data}
   <CalendarNotice {data} onfilters={onFilters} onlist={onList} onretry={() => retry++} />
   {#if !rows.length && !incomplete(data)}
    {#if Object.keys(filters).length}<p>No entries match these filters.</p><Button onclick={onClearFilters}>Clear filters</Button>
    {:else}<p>No entries this month.</p><p>Choose another month or date field.</p>{/if}
   {/if}
   {#if viewport.isMobile}
    <ol class="day-list" aria-label={monthLabel(month)}>
     {#each days as day}
      <li class:today={day===today}>
       <h3><button class="day-header" data-day={day} aria-current={day===today ? 'date' : undefined} onclick={() => openDay(day)}>{dayLabel(day,true)}{day===today ? ' · Today' : ''}<span>{incompleteDay(data,day) ? 'Not fully loaded' : byDay.get(day)?.length ? `${byDay.get(day)!.length} entries` : 'No entries'}</span></button></h3>
       {@render entries(day,true)}
      </li>
     {/each}
    </ol>
   {:else}
    <p id="{uid}-help" class="instructions">Use arrow keys to choose a day. Press Enter to open its entries.</p>
    <div role="grid" aria-label={monthLabel(month)} aria-describedby="{uid}-help" class="month-grid">
     <div role="row" class="weekdays">{#each ['Monday','Tuesday','Wednesday','Thursday','Friday','Saturday','Sunday'] as name}<div role="columnheader" aria-label={name}>{name.slice(0,3)}</div>{/each}</div>
     {#each weeks as week}
      <div role="row" class="week">
       {#each week as day}
        <div role="gridcell" class="cell" class:blank={!day} class:today={day===today} aria-selected={day ? selected===day : undefined}>
         {#if day}
          <button class="date" data-day={day} tabindex={day===focusDay ? 0 : -1} aria-label={cellLabel(day)} aria-current={day===today ? 'date' : undefined} onkeydown={(e) => keydown(e,day)} onclick={() => openDay(day)}>{Number(day.slice(-2))}{#if day===today}<span>Today</span>{/if}</button>
          {@render entries(day,false)}
         {/if}
        </div>
       {/each}
      </div>
     {/each}
    </div>
   {/if}
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
 .week, .weekdays { display:grid; grid-template-columns:repeat(7,minmax(0,1fr)); }
 .weekdays { color:var(--text-secondary); text-align:center; font-size:12px; padding:var(--space-2) 0; }
 .month-grid { border:1px solid var(--border); border-radius:var(--radius); overflow:hidden; }
 .cell { min-height:144px; min-width:0; padding:var(--space-2); border-top:1px solid var(--border); border-right:1px solid var(--border); display:flex; flex-direction:column; gap:var(--space-1); }
 .cell:last-child { border-right:0; }
 .blank { background:var(--bg-secondary); }
 .today { box-shadow:inset 0 0 0 2px var(--accent-primary); }
 .cell[aria-selected="true"] { background:var(--bg-active); }
 button { font:inherit; color:var(--text-primary); border:0; background:transparent; cursor:pointer; border-radius:var(--radius-sm); }
 button:hover { background:var(--bg-hover); }
 button:focus-visible { outline:2px solid var(--accent-primary); outline-offset:-2px; }
 .date { align-self:flex-start; padding:var(--space-1); min-width:28px; text-align:left; font-size:12px; }
 .date span { margin-left:var(--space-1); }
 .more { text-align:left; font-size:12px; padding:var(--space-1); }
 .instructions, .incomplete-day { color:var(--text-secondary); font-size:12px; margin:var(--space-2) 0; }
 .day-list { list-style:none; padding:0; margin:0; }
 .day-list li { border-top:1px solid var(--border); padding:var(--space-2); }
 h3 { margin:0; }
 .day-header { width:100%; min-height:44px; display:flex; justify-content:space-between; flex-wrap:wrap; gap:var(--space-2); text-align:left; font-size:14px; }
 .day-header span { color:var(--text-secondary); font-weight:normal; }
 .sr-only { position:absolute; width:1px; height:1px; overflow:hidden; clip-path:inset(50%); }
 @media (pointer:coarse) { button, .calendar :global(.btn) { min-height:44px; min-width:44px; } }
 @media (max-width:768px) { .calendar { padding:var(--space-2); } }
</style>
