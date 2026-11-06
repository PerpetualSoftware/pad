<script lang="ts">
 import type { Collection } from '$lib/types';
 import { dateFields, type CalendarConfig } from '$lib/calendar/presentation';
 import Button from '../common/Button.svelte';
 let {collection, config, onchange, onclose}: {collection:Collection;config:CalendarConfig;onchange:(config:CalendarConfig)=>void;onclose?:()=>void} = $props();
 const uid = $props.id();
 let fields = $derived(dateFields(collection));
</script>
<div class="picker">
 <label for="{uid}-date">Date field</label>
 <select id="{uid}-date" value={config.date_field ?? ''} onchange={(e) => onchange({...config,date_field:e.currentTarget.value})}>
  <option value="" disabled>Choose a date field</option>
  <option value="created">Created</option><option value="completed">Completed</option>
  {#each fields.filter(f => f.key !== 'created' && f.key !== 'completed') as field (field.key)}<option value={field.key}>{field.label}</option>{/each}
 </select>
 <label for="{uid}-end">End field (optional)</label>
 <select id="{uid}-end" value={config.end_field ?? ''} onchange={(e) => onchange({...config,end_field:e.currentTarget.value || undefined})}>
  <option value="">None</option>
  {#each fields as field (field.key)}<option value={field.key}>{field.label}</option>{/each}
 </select>
 {#if onclose}<Button onclick={onclose}>Done</Button>{/if}
</div>
<style>
 .picker { display:flex; flex-direction:column; gap:var(--space-2); padding:var(--space-4); }
 label { color:var(--text-secondary); }
 select { width:100%; min-height:44px; background:var(--bg-tertiary); color:var(--text-primary); border:1px solid var(--border); border-radius:var(--radius); padding:var(--space-2); font:inherit; }
 select:focus-visible { outline:2px solid var(--accent-primary); outline-offset:2px; }
</style>
