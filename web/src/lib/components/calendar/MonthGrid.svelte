<script lang="ts">
 import { tick, type Snippet } from 'svelte';
 import { dayLabel, monthDays, monthLabel, moveMonth, shiftDay } from '$lib/calendar/monthGrid';
 let {month, today, selected, mobile, cellLabel, summary, onopen, onchoose, loaded, cell}: {
  month:string; today:string; selected:string; mobile:boolean;
  cellLabel:(day:string)=>string; summary:(day:string)=>string;
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
     {#each days as day}
      <li class:today={day===today}>
       <h3><button class="day-header" data-day={day} aria-current={day===today ? 'date' : undefined} onclick={() => onopen(day)}>{dayLabel(day,true)}{day===today ? ' · Today' : ''}<span>{summary(day)}</span></button></h3>
       {@render cell(day,true)}
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
          <button class="date" data-day={day} tabindex={day===focusDay ? 0 : -1} aria-label={cellLabel(day)} aria-current={day===today ? 'date' : undefined} onkeydown={(e) => keydown(e,day)} onclick={() => onopen(day)}>{Number(day.slice(-2))}{#if day===today}<span>Today</span>{/if}</button>
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
 .instructions { color:var(--text-secondary); font-size:12px; margin:var(--space-2) 0; }
 .day-list { list-style:none; padding:0; margin:0; }
 .day-list li { border-top:1px solid var(--border); padding:var(--space-2); }
 h3 { margin:0; }
 .day-header { width:100%; min-height:44px; display:flex; justify-content:space-between; flex-wrap:wrap; gap:var(--space-2); text-align:left; font-size:14px; }
 .day-header span { color:var(--text-secondary); font-weight:normal; }
 @media (pointer:coarse) { button { min-height:44px; min-width:44px; } }
</style>
