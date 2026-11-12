<script lang="ts">
 import { tick, type Snippet } from 'svelte';
 import { dayLabel, dayRuns, emptyRunLabel, monthDays, monthLabel, moveMonth, shiftDay } from '$lib/calendar/monthGrid';
 let {month, today, selected, mobile, cellLabel, summary, heading, count = () => '', empty = () => false, onopen, onchoose, loaded, cell}: {
  month:string; today:string; selected:string; mobile:boolean;
  cellLabel:(day:string)=>string; summary:(day:string)=>string;
  /** The phone list's own words for a day, such as "Thursday 12 November"; today then carries a badge. Without it a day reads as its full date. */
  heading?:(day:string)=>string;
  /** The text beside a date in the grid, such as "6 · 2 owed". Empty shows nothing. */
  count?:(day:string)=>string;
  /** Whether a day is fully loaded and has no entries. The phone list folds runs of such days into one line. */
  empty?:(day:string)=>boolean;
  onopen:(day:string)=>void; onchoose:(day:string)=>void;
  /** Null hides the grid during loading; a new result completes pending focus. */
  loaded:unknown; cell:Snippet<[string, boolean]>;
 } = $props();
 const uid = $props.id();
 let root: HTMLDivElement;
 // Plain closure state: focus changes must not invalidate the data effect.
 let pendingFocus: string | null = null;
 let days = $derived(monthDays(month));
 let focusDay = $derived(selected.startsWith(month) ? selected : today.startsWith(month) ? today : `${month}-01`);
 let leading = $derived((new Date(`${month}-01T12:00:00Z`).getUTCDay() + 6) % 7);
 let cells = $derived([...Array<string|null>(leading).fill(null), ...days, ...Array<string|null>((7 - (leading + days.length) % 7) % 7).fill(null)]);
 let weeks = $derived(Array.from({length:cells.length/7}, (_, i) => cells.slice(i*7,i*7+7)));
 // Today, the selected day and the day that holds focus always keep their own header.
 let runs = $derived(dayRuns(days, day => day !== today && day !== selected && day !== focusDay && empty(day)));
 export function cancelFocus() { pendingFocus = null; }
 async function finishFocus() {
  const day = pendingFocus;
  if (!day) return;
  await tick();
  if (pendingFocus !== day) return;
  const el = root?.querySelector<HTMLButtonElement>(`[data-day="${day}"]`);
  if (el) { el.focus(); pendingFocus = null; }
 }
 export function focusDate(day: string) { pendingFocus = day; void finishFocus(); }
 function choose(day: string) { onchoose(day); focusDate(day); }
 $effect(() => { loaded; mobile; void finishFocus(); });
 $effect.pre(() => {
  mobile;
  if (root?.contains(document.activeElement) && document.activeElement instanceof HTMLElement && document.activeElement.dataset.day) pendingFocus = document.activeElement.dataset.day;
 });
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
  event.preventDefault(); event.stopPropagation(); choose(target);
 }
</script>

<div bind:this={root}>
 {#if loaded}
   {#if mobile}
    <ol class="day-list" aria-label={monthLabel(month)}>
     {#each runs as run (run.from)}
      {#if run.empty}
       <li class="empty-run">{emptyRunLabel(run.from, run.to)}</li>
      {:else}
       {@const day = run.from}
       <li class:today={day===today}>
        <h3><button class="day-header" data-day={day} aria-current={day===today ? 'date' : undefined} onclick={() => onopen(day)}>{#if heading}{heading(day)}{#if day===today}{' '}<i class="badge">Today</i>{/if}{:else}{dayLabel(day,true)}{day===today ? ' · Today' : ''}{/if}{' '}<span>{summary(day)}</span></button></h3>
        {@render cell(day,true)}
       </li>
      {/if}
     {/each}
    </ol>
   {:else}
    <p id="{uid}-help" class="sr-only">Use arrow keys to choose a day. Press Enter to open its entries.</p>
    <div role="grid" aria-label={monthLabel(month)} aria-describedby="{uid}-help" class="month-grid">
     <div role="row" class="weekdays">{#each ['Monday','Tuesday','Wednesday','Thursday','Friday','Saturday','Sunday'] as name}<div role="columnheader" aria-label={name}>{name.slice(0,3)}</div>{/each}</div>
     {#each weeks as week}
      <div role="row" class="week">
       {#each week as day}
        <div role="gridcell" class="cell" class:blank={!day} class:today={day===today} aria-selected={day ? selected===day : undefined}>
         {#if day}
          <div class="head">
           <button class="date" data-day={day} tabindex={day===focusDay ? 0 : -1} aria-label={cellLabel(day)} aria-current={day===today ? 'date' : undefined} onkeydown={(e) => keydown(e,day)} onclick={() => onopen(day)}><b>{Number(day.slice(-2))}</b></button>
           {#if count(day)}<span class="count" aria-hidden="true">{count(day)}</span>{/if}
          </div>
          {@render cell(day,false)}
         {/if}
        </div>
       {/each}
      </div>
     {/each}
    </div>
   {/if}
 {/if}
</div>
<style>
 .week, .weekdays { display:grid; grid-template-columns:repeat(7,minmax(0,1fr)); }
 .weekdays { color:var(--text-muted); font-size:11px; font-weight:600; letter-spacing:.04em; text-transform:uppercase; border-bottom:1px solid var(--border); }
 .weekdays div { padding:6px 10px; }
 .month-grid { border:1px solid var(--card-border); border-radius:var(--radius-lg); overflow:hidden; background:var(--card-bg); }
 /* Six week rows and the header fit a 900px tall window. */
 .cell { min-height:118px; min-width:0; padding:6px; border-right:1px solid var(--border-subtle); border-bottom:1px solid var(--border-subtle); display:flex; flex-direction:column; gap:2px; }
 .cell:last-child { border-right:0; }
 .week:last-child .cell { border-bottom:0; }
 .blank { background:color-mix(in srgb, var(--bg-tertiary) 50%, transparent); }
 .cell[aria-selected="true"] { box-shadow:inset 0 0 0 2px var(--accent-primary); }
 button { font:inherit; color:var(--text-primary); border:0; background:transparent; cursor:pointer; border-radius:var(--radius-sm); }
 button:hover { background:var(--bg-hover); }
 button:focus-visible { outline:2px solid var(--accent-primary); outline-offset:-2px; }
 .head { display:flex; align-items:center; justify-content:space-between; gap:var(--space-1); margin-bottom:2px; }
 .date { padding:0; border-radius:50%; }
 .date b { display:grid; place-items:center; width:22px; height:22px; border-radius:50%; font-size:12px; font-weight:600; font-variant-numeric:tabular-nums; }
 .today .date b { background:var(--accent-primary-strong); color:var(--text-on-accent); }
 .count { font-size:10.5px; color:var(--text-muted); white-space:nowrap; }
 .sr-only { position:absolute; width:1px; height:1px; overflow:hidden; clip-path:inset(50%); white-space:nowrap; }
 .day-list { list-style:none; padding:0; margin:0; }
 .day-list li { padding:var(--space-2) 0; }
 .day-list .empty-run { padding:6px 4px; color:var(--text-muted); font-size:13px; }
 h3 { margin:0; }
 .day-header { width:100%; min-height:44px; display:flex; align-items:center; justify-content:space-between; flex-wrap:wrap; gap:var(--space-2); padding:0 4px; text-align:left; font-size:15px; font-weight:700; }
 .day-header span { margin-left:auto; color:var(--text-secondary); font-size:12px; font-weight:normal; }
 .badge { font-style:normal; font-size:11px; font-weight:700; color:var(--text-on-accent); background:var(--accent-primary-strong); border-radius:99px; padding:0 8px; }
 @media (pointer:coarse) { button { min-height:44px; min-width:44px; } }
</style>
