<script lang="ts">
 import { monthLabel } from '$lib/calendar/monthGrid';
 import Button from '../common/Button.svelte';
 let {month, zone, zoneSet, dateLabel, onprevious, onnext, ontoday, onfields}: {month:string;zone:string;zoneSet:boolean;dateLabel:string;onprevious:()=>void;onnext:()=>void;ontoday:()=>void;onfields:()=>void} = $props();
</script>
<div class="toolbar">
 <Button onclick={onfields}>Date: {dateLabel}</Button>
 <div class="navigation"><Button aria-label="Previous month" onclick={onprevious}>‹</Button><h2>{monthLabel(month)}</h2><Button aria-label="Next month" onclick={onnext}>›</Button><Button onclick={ontoday}>Today</Button></div>
 <a href="/settings/account">{zoneSet ? `Time zone: ${zone}` : 'UTC · Set time zone'}</a>
</div>
<style>
 .toolbar { display:flex; align-items:center; flex-wrap:wrap; gap:var(--space-3); margin-bottom:var(--space-4); }
 .navigation { display:flex; align-items:center; gap:var(--space-2); flex-wrap:wrap; }
 h2 { font-size:16px; margin:0; }
 a { color:var(--text-secondary); font-size:12px; overflow-wrap:anywhere; }
 @media (pointer:coarse) { .toolbar :global(button), a { min-height:44px; } }
</style>
