<script lang="ts">
	import Chip from '$lib/components/common/Chip.svelte';
	import OccurrenceActions from './OccurrenceActions.svelte';
	import { canResolve, dayLabel, entryChips, entryHref, entryKey, isAllDay, localTime } from '$lib/calendar/presentation';
	import type { AgendaEntry, ScheduleResolveState } from '$lib/types';

	let { day, entries, zone, pending, saved, onaction }: {
		day: string; entries: AgendaEntry[]; zone: string;
		pending: Record<string, boolean>; saved: Record<string, boolean>;
		onaction: (entry: AgendaEntry, state: ScheduleResolveState) => void;
	} = $props();
</script>

<section>
	<h2>{dayLabel(day)}</h2>
	<ul>
		{#each entries as entry (entryKey(entry))}
			{@const href = entryHref(entry)}
			{@const done = entry.type === 'schedule' && entry.state === 'done'}
			<li class="agenda-row" data-ref={entry.item_ref}>
				<div class="time">{isAllDay(entry) ? 'All day' : localTime(entry.at, zone)}</div>
				<div class="main">
					<div class="title" class:done>
						{#if done}<span aria-label="Done">✓</span>{/if}
						{#if href}<a {href}>{entry.item_title}</a>{:else}<span>{entry.item_title}</span>{/if}
					</div>
					<div class="meta"><span>{entry.workspace_name} / {entry.collection}</span><span>{entry.item_ref}</span></div>

				</div>
				<div class="chips">
					{#each entryChips(entry) as chip, index (index)}
						<Chip size="sm" color={chip.color} dot><span class="chip-text">{chip.text}</span></Chip>
					{/each}
				</div>
				{#if canResolve(entry)}
					<OccurrenceActions {entry} pending={pending[entryKey(entry)]} disabled={saved[entryKey(entry)]} {onaction} />
				{/if}
			</li>
		{/each}
	</ul>
</section>

<style>
	h2 { font-size: 1.1rem; margin: var(--space-4) 0 var(--space-2); }
	ul { padding: 0; margin: 0; }
	.agenda-row { display: flex; align-items: flex-start; gap: var(--space-4); padding: var(--space-3) var(--space-4); border-bottom: 1px solid var(--border); list-style: none; }
	.time { flex: 0 0 4rem; color: var(--text-secondary); font-size: 0.85rem; font-variant-numeric: tabular-nums; }
	.main { min-width: 0; flex: 1; overflow-wrap: anywhere; }
	.title { font-weight: 600; color: var(--text-primary); }
	.title a { color: inherit; text-decoration: none; }
	.title a:hover { text-decoration: underline; }
	.done a, .done > span:last-child { text-decoration: line-through; }
	.meta { display: flex; flex-wrap: wrap; gap: var(--space-2); margin-top: var(--space-1); font-size: 0.8rem; color: var(--text-muted); }
	.chips { display: flex; flex-wrap: wrap; gap: var(--space-1); max-width: 16rem; }
	.chip-text { color: var(--text-primary); }
	@media (max-width: 767px) {
		.agenda-row { flex-direction: column; gap: var(--space-2); padding: var(--space-3) 0; }
		.time { flex: none; order: 1; }
		.main { order: 0; width: 100%; }
		.chips { order: 2; max-width: 100%; }
		.agenda-row :global(.actions) { order: 3; }
	}
</style>
