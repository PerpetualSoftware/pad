<script lang="ts">
 import { dayLabel, timeLabel, type CalendarRow } from '$lib/calendar/monthGrid';
 import { shouldOpenInPane } from '../collections/itemCardClick';
 import { canonicalValueColor, formatFieldLabel } from '$lib/utils/fieldColors';
 import { acknowledgedText, forecastText, occurrenceState, type OccurrenceState } from '$lib/calendar/presentation';
 import Chip from '../common/Chip.svelte';
 import StateMark from './StateMark.svelte';
 let {row, zone, basePath = '', onopen, preview = false, keyboard = true}: {row:CalendarRow;zone:string;basePath?:string;onopen?:(row:CalendarRow)=>void;preview?:boolean;keyboard?:boolean} = $props();
 let state = $derived(occurrenceState(row.state));
 let done = $derived((!!row.terminal && !row.abandoned) || row.state === 'done');
 let closed = $derived(done || !!row.abandoned);
 // The mark: the occurrence state, a closed item's check or dash under its own status name, else the dated-field square.
 let mark = $derived<{state?:OccurrenceState;label?:string}>(state ? {state: row.state as OccurrenceState}
  : row.terminal ? {state: row.abandoned ? 'skipped' : 'done', label: formatFieldLabel(row.terminal)}
  : {label: row.kind === 'forecast' ? 'Forecast' : row.label ?? 'Date'});
 let bad = $derived(state?.tone === '--accent-red');
 let acknowledged = $derived(row.state === 'acknowledged' ? acknowledgedText(row.acknowledgedAt, row.acknowledgedBy, zone) : '');
 let time = $derived(row.allDay || !row.at ? '' : timeLabel(row.at, zone));
 let name = $derived(row.continues && preview ? 'Continues' : row.title);
 // Everything the one-line preview leaves out, for its tooltip and accessible name.
 // A forecast marker is an estimate, not the entry itself: it says so first.
 let tip = $derived([row.kind === 'forecast' ? 'Forecast' : '', row.title, time || 'All day', row.label, state?.label ?? (row.terminal ? formatFieldLabel(row.terminal) : ''),
  row.overdue ? 'Overdue' : '', acknowledged, row.workspaceName].filter(Boolean).join(' · '));
 let href = $derived(row.href === undefined ? `${basePath}/${encodeURIComponent(row.target)}` : row.href ?? undefined);
 function click(e: MouseEvent) { if (onopen && shouldOpenInPane(e,true)) {e.preventDefault();onopen(row);} }
</script>
{#if preview}
 <a class="line" class:forecast-marker={row.kind === 'forecast'} data-role={row.kind === 'forecast' ? 'forecast' : undefined} class:attention={row.attention} class:closed class:struck={row.abandoned} {href} title={tip} aria-label={tip} tabindex={keyboard ? 0 : -1} onclick={click}>
  <StateMark {...mark} overdue={!!row.overdue || !!row.attention} />{#if time}<time>{time}</time>{/if}<span class="t">{name}</span>
 </a>
{:else}
<a class="entry" class:forecast-marker={row.kind === 'forecast'} data-role={row.kind === 'forecast' ? 'forecast' : undefined} class:closed class:struck={row.abandoned} class:bad {href} tabindex={keyboard ? 0 : -1} onclick={click}>
 <span class="title"><StateMark {...mark} overdue={!!row.overdue} /> <span class="ref">{row.ref}</span> <span class="name">{name}</span></span>
 <span class="meta">
  {#if row.allDay}<span>All day</span>{:else if time}<span>{time}</span>{/if}
  {#if row.workspaceName}<span>{row.workspaceName}</span>{/if}
  {#if row.label}<span>{row.label}</span>{/if}
  {#if row.terminal}<Chip size="sm" color={row.abandoned ? 'var(--accent-gray)' : (canonicalValueColor(row.terminal) ?? 'var(--accent-green)')}>{formatFieldLabel(row.terminal)}</Chip>{/if}
  {#if state}<Chip size="sm" color={`var(${state.tone})`}>{state.label}</Chip>{/if}
  {#if acknowledged}<span>{acknowledged}</span>{/if}
  {#if row.overdue}<Chip size="sm" color="var(--accent-red)">Overdue</Chip>{/if}
  {#if row.stale}<Chip size="sm" color="var(--accent-amber)">Stale</Chip>{/if}
 </span>
 {#if row.start && row.end}<span class="range">{row.continues ? '‹ ' : ''}{dayLabel(row.start)} – {dayLabel(row.end)}{row.day < row.end ? ' ›' : ''}</span>{/if}

 {#if row.forecast}<span class="forecast" data-role={row.kind === 'forecast' ? undefined : 'forecast'}>{forecastText(row.forecast, zone)}</span>{/if}
 {#if row.missedCount}<span class="range">{row.missedCount} missed occurrences</span>{/if}
</a>
{/if}
<style>
 .forecast, .forecast-marker { border: 1px dashed var(--border-strong); }
 .forecast { display: block; padding: var(--space-1); font-size: 0.85rem; }
 .forecast-marker .forecast { border: 0; }
 .entry { display:block; padding:var(--space-2); border-radius:var(--radius-sm); color:var(--text-primary); text-decoration:none; min-width:0; overflow-wrap:anywhere; }
 .entry:hover, .line:hover { background:var(--bg-hover); }
 .entry:focus-visible, .line:focus-visible { outline:2px solid var(--accent-primary); outline-offset:-2px; }
 .title { display:block; }
 .ref { font-family:var(--font-mono); color:var(--text-secondary); }
 /* Done reads as finished, abandoned as withdrawn: only the second is struck through. */
 .closed .name, .closed .t { color:var(--text-muted); }
 .struck .name, .struck .t { text-decoration:line-through; text-decoration-thickness:1px; }
 .bad .name { color:var(--accent-red); }
 .meta { display:flex; align-items:center; flex-wrap:wrap; gap:var(--space-1); color:var(--text-secondary); }
 .range { display:block; color:var(--text-secondary); font-size:12px; }
 /* The month cell line: mark, time, title. */
 .line { --state-mark-size:9px; display:flex; align-items:center; gap:5px; min-width:0; padding:1px 4px; border-radius:4px; font-size:12px; line-height:1.55; color:var(--text-primary); text-decoration:none; }
 .line time { flex:0 0 auto; color:var(--text-muted); font-size:11px; font-variant-numeric:tabular-nums; }
 .t { overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
 .attention .t { color:var(--accent-red); font-weight:600; }
 @media (pointer:coarse) { .entry, .line { min-height:44px; } }
 @media (max-width:768px) { .line { font-size:14px; } .line time { font-size:13px; } }
</style>
