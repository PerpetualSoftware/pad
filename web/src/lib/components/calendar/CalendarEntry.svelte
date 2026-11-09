<script lang="ts">
 import { dayLabel, timeLabel, zoneDay, type CalendarRow } from '$lib/calendar/monthGrid';
 import { shouldOpenInPane } from '../collections/itemCardClick';
 import { canonicalValueColor, formatFieldLabel } from '$lib/utils/fieldColors';
 import Chip from '../common/Chip.svelte';
 let {row, zone, basePath, onopen, preview = false, keyboard = true}: {row:CalendarRow;zone:string;basePath:string;onopen:(row:CalendarRow)=>void;preview?:boolean;keyboard?:boolean} = $props();
 const states: Record<string,[string,string]> = {done:['Done','green'],skipped:['Skipped','gray'],moved:['Moved','purple'],missed:['Missed','amber'],lapsed:['Lapsed','gray'],outstanding:['Outstanding','amber'],scheduled:['Scheduled','blue'],unrecorded:['No recorded state','gray']};
 let state = $derived(row.state ? states[row.state] : undefined);
 let done = $derived((!!row.terminal && !row.abandoned) || row.state === 'done');
 let closed = $derived(done || !!row.abandoned);
</script>
<a class="entry" class:preview class:done={closed} href="{basePath}/{encodeURIComponent(row.target)}" tabindex={keyboard ? 0 : -1} onclick={(e) => { if (shouldOpenInPane(e,true)) {e.preventDefault();onopen(row);} }}>
 <span class="title">{#if done}<span aria-label="Done">✓ </span>{:else if row.abandoned}<span aria-hidden="true">– </span>{/if}<span class="ref">{row.ref}</span> <span class="name">{row.continues && preview ? 'Continues' : row.title}</span></span>
 <span class="meta">
  {#if row.allDay}<span>All day</span>{:else if row.at}<span>{timeLabel(row.at,zone)}</span>{/if}
  {#if row.label}<span>{row.label}</span>{/if}
  {#if row.terminal}<Chip size="sm" color={row.abandoned ? 'var(--accent-gray)' : (canonicalValueColor(row.terminal) ?? 'var(--accent-green)')}>{formatFieldLabel(row.terminal)}</Chip>{/if}
  {#if state}<Chip size="sm" color={row.state === 'scheduled' ? 'var(--status-blue)' : `var(--accent-${state[1]})`}>{state[0]}</Chip>{/if}
  {#if row.overdue}<Chip size="sm" color="var(--accent-red)">Overdue</Chip>{/if}
  {#if row.stale}<Chip size="sm" color="var(--accent-amber)">Stale</Chip>{/if}
 </span>
 {#if row.start && row.end}<span class="range">{row.continues ? '‹ ' : ''}{dayLabel(row.start)} – {dayLabel(row.end)}{row.day < row.end ? ' ›' : ''}</span>{/if}
 {#if row.state === 'moved' && row.effectiveAt}<span class="range">Moved to {dayLabel(zoneDay(new Date(row.effectiveAt),zone))}, {timeLabel(row.effectiveAt,zone)}</span>{/if}
 {#if row.missedCount}<span class="range">{row.missedCount} missed occurrences</span>{/if}
</a>
<style>
 .entry { display:block; padding:var(--space-2); border-radius:var(--radius-sm); color:var(--text-primary); text-decoration:none; min-width:0; overflow-wrap:anywhere; }
 .entry:hover { background:var(--bg-hover); }
 .entry:focus-visible { outline:2px solid var(--accent-primary); outline-offset:-2px; }
 .preview { font-size:12px; padding:var(--space-1); background:var(--bg-secondary); }
 .title { display:block; }
 .preview .title { overflow:hidden; display:-webkit-box; -webkit-box-orient:vertical; -webkit-line-clamp:2; line-clamp:2; }
 .ref { font-family:var(--font-mono); color:var(--text-secondary); }
 .done .name { text-decoration:line-through; text-decoration-thickness:1px; }
 .meta { display:flex; align-items:center; flex-wrap:wrap; gap:var(--space-1); color:var(--text-secondary); }
 .range { display:block; color:var(--text-secondary); font-size:12px; }
 .preview .range { white-space:nowrap; overflow:hidden; text-overflow:ellipsis; }
 @media (pointer:coarse) { .entry { min-height:44px; } }
 @media (max-width:768px) { .preview { font-size:14px; } }
</style>
