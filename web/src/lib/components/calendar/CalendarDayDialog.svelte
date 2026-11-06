<script lang="ts">
 import type { CalendarResponse } from '$lib/types';
 import { dayLabel, incompleteDay, type CalendarRow } from '$lib/calendar/presentation';
 import { viewport } from '$lib/stores/breakpoint.svelte';
 import Modal from '../common/Modal.svelte';
 import BottomSheet from '../common/BottomSheet.svelte';
 import Button from '../common/Button.svelte';
 import CalendarEntry from './CalendarEntry.svelte';
 import CalendarNotice from './CalendarNotice.svelte';
 let {day, rows, data, zone, basePath, onclose, onopen, onfilters, onlist}: {day:string|null;rows:CalendarRow[];data:CalendarResponse;zone:string;basePath:string;onclose:()=>void;onopen:(row:CalendarRow)=>void;onfilters:()=>void;onlist:()=>void} = $props();
 const uid = $props.id();
</script>
{#snippet body()}
 <div class="detail">
  {#if !viewport.isMobile}<div class="heading"><h2 id={uid}>{day ? dayLabel(day,true) : ''}</h2><Button onclick={onclose}>Close</Button></div>{/if}
  <p>Time zone: {zone}</p>
  <div class="entries">
   <CalendarNotice {data} {onfilters} {onlist} />
   {#if day}<CalendarNotice {data} {day} {onfilters} {onlist} />{/if}
   {#if day && incompleteDay(data,day)}<p>Not fully loaded</p>{/if}
   {#if rows.length === 0 && day && !incompleteDay(data,day)}<p>No entries on this day.</p>{/if}
   {#each rows as row (row.key)}<CalendarEntry {row} {zone} {basePath} {onopen} />{/each}
  </div>
 </div>
{/snippet}
{#if viewport.isMobile}
 {#if day}<BottomSheet open={true} {onclose} title={dayLabel(day,true)}>{@render body()}</BottomSheet>{/if}
{:else}
 <Modal open={!!day} {onclose} labelledby={uid} maxWidth="640px">{@render body()}</Modal>
{/if}
<style>
 .detail { padding:var(--space-4); }
 .heading { display:flex; align-items:flex-start; justify-content:space-between; gap:var(--space-2); }
 h2 { font-size:18px; margin:0; } p { color:var(--text-secondary); }
 .entries { max-height:65vh; overflow:auto; overscroll-behavior:contain; }
 @media (pointer:coarse) { .heading :global(button) { min-height:44px; } }
</style>
