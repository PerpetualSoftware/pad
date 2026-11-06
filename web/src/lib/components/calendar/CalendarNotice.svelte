<script lang="ts">
 import type { CalendarResponse } from '$lib/types';
 import { dayLabel, PARTIAL_DAY } from '$lib/calendar/presentation';
 import Button from '../common/Button.svelte';
 let {data, day, onfilters, onlist, onretry, keyboard = true}: {data:CalendarResponse;day?:string;onfilters:()=>void;onlist:()=>void;onretry?:()=>void;keyboard?:boolean} = $props();
</script>
{#if day && data.partial_day === day}
 <div class="notice"><p>{PARTIAL_DAY}</p><div class="actions"><Button tabindex={keyboard ? 0 : -1} size="sm" onclick={onfilters}>Filters</Button><Button tabindex={keyboard ? 0 : -1} size="sm" onclick={onlist}>Use list</Button></div></div>
{/if}
{#if !day && data.truncated}
 <div class="notice">
  {#if data.next_from}<p>Calendar is incomplete. <span>more after {dayLabel(data.next_from)}</span></p>
  {:else}<p>Calendar is incomplete. Filter the calendar, or use the list.</p>{/if}
  <div class="actions"><Button tabindex={keyboard ? 0 : -1} size="sm" onclick={onfilters}>Filters</Button><Button tabindex={keyboard ? 0 : -1} size="sm" onclick={onlist}>Use list</Button></div>
 </div>
{/if}
{#if !day && (data.unexpanded?.length || data.unavailable?.length)}
 <div class="notice"><details><summary>Some schedules could not be shown.</summary>
  <ul>{#each data.unexpanded ?? [] as entry}<li>{entry.item_ref} {entry.item_title}: {entry.reason}</li>{/each}
  {#each data.unavailable ?? [] as entry}<li>{entry.item_ref}: {entry.code}</li>{/each}</ul>
 </details>{#if onretry}<Button tabindex={keyboard ? 0 : -1} size="sm" onclick={onretry}>Try again</Button>{/if}</div>
{/if}
<style>
 .notice { padding:var(--space-2); background:var(--bg-secondary); border:1px solid var(--border); border-radius:var(--radius-sm); color:var(--text-primary); font-size:12px; overflow-wrap:anywhere; }
 p { margin:0 0 var(--space-2); }
 .actions { display:flex; flex-wrap:wrap; gap:var(--space-2); }
 @media (pointer:coarse) { .actions :global(button) { min-height:44px; } }
</style>
