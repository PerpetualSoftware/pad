<script lang="ts">
	import AgendaRow from './AgendaRow.svelte';
	import { dayLabel, entryKey, groupEntries, todayIn, shiftDay, type AgendaAction } from '$lib/calendar/presentation';
	import type { AgendaEntry, CommitmentMoveResult } from '$lib/types';
	let { start, entries, zone, complete = true, pending, saved, dragging, impact, titles, onaction, ondragstart, ondragend, onenter, ondrop }: {
		start: string; entries: AgendaEntry[]; zone: string; complete?: boolean; pending: Record<string, boolean>; saved: Record<string, boolean>;
		dragging: AgendaEntry | null; impact: CommitmentMoveResult | null; titles: Record<string, string>;
		onaction: (entry: AgendaEntry, action: AgendaAction) => void;
		ondragstart: (entry: AgendaEntry, event: DragEvent) => void; ondragend: () => void;
		onenter: (day: string) => void; ondrop: (day: string) => void;
	} = $props();
	const days = $derived(Array.from({ length: 7 }, (_, index) => shiftDay(start, index)));
	const groups = $derived(groupEntries(entries));
	const localDay = (at: string) => todayIn(zone, new Date(at));
	const weekday = (day: string) => new Intl.DateTimeFormat('en-GB', { weekday: 'long', timeZone: 'UTC' }).format(new Date(`${day}T12:00Z`));
</script>

<div class="week">
	{#each days as day (day)}
		{@const rows = groups.find(group => group.day === day)?.entries ?? []}
		<section data-day={day} aria-label={dayLabel(day)}
			ondragenter={event => { if (dragging) { event.preventDefault(); onenter(day); } }}
			ondragover={event => { if (dragging) { event.preventDefault(); if (event.dataTransfer) event.dataTransfer.dropEffect = 'move'; } }}
			ondrop={event => { event.preventDefault(); ondrop(day); }}>
			<h2>{weekday(day)}, {dayLabel(day)}{day === todayIn(zone) ? ' · Today' : ''}</h2>
			{#if dragging?.roles?.earliest && localDay(dragging.roles.earliest) === day}<p class="pinned">Available from</p>{/if}
			{#if dragging?.roles?.deadline && localDay(dragging.roles.deadline) === day}<p class="pinned">Deadline</p>{/if}
			<ul>
				{#each rows as entry (entryKey(entry))}
					<AgendaRow {entry} {zone} week pending={pending[entryKey(entry)]} saved={saved[entryKey(entry)]} {onaction} {ondragstart} {ondragend} />
				{/each}
				{#each entries.filter(entry => entry.type === 'schedule' && entry.roles?.deadline && localDay(entry.roles.deadline) === day && entry.day !== day) as entry, index (index)}
					<li class="marker">
						<span>Deadline · {entry.item_title}</span>
						<div class="meta">{entry.workspace_name} / {entry.collection} / {entry.item_ref}</div>
					</li>
				{/each}
				{#each impact?.changes.filter(change => !change.root && localDay(change.to) === day) ?? [] as change, index (index)}
					<li class="ghost" aria-hidden="true">
						<span>{titles[change.item_id] ?? 'Linked item'} (linked) · proposed</span>
						{#if impact?.violations?.some(violation => violation.item_id === change.item_id && violation.role === change.role)}<span>Outside its bound</span>{/if}
					</li>
				{/each}
			</ul>
			{#if rows.length === 0 && complete}<p class="empty">Nothing on this day.</p>{/if}
		</section>
	{/each}
</div>
{#if impact?.renegotiates_deadline}<p>Review needed: this move changes a hard deadline.</p>{/if}

<style>
	.week { display: grid; grid-template-columns: repeat(7, minmax(0, 1fr)); border: 1px solid var(--border); }
	section { min-width: 0; border-right: 1px solid var(--border); padding-bottom: var(--space-3); }
	section:last-child { border-right: 0; }
	h2 { font-size: 0.9rem; padding: var(--space-3); margin: 0; border-bottom: 1px solid var(--border); }
	ul { list-style: none; padding: 0; margin: 0; }
	.marker, .ghost { padding: var(--space-3); font-size: 0.85rem; overflow-wrap: anywhere; }
	.meta, .empty { color: var(--text-muted); font-size: 0.8rem; }
	.empty, .pinned { padding: 0 var(--space-3); }
	.pinned { font-weight: 600; font-size: 0.85rem; }
	.ghost { border: 1px dashed var(--border-strong); margin: var(--space-2); }
	.ghost span { display: block; }
	@media (max-width: 768px) {
		.week { grid-template-columns: minmax(0, 1fr); }
		section { border-right: 0; border-bottom: 1px solid var(--border); }
		section:last-child { border-bottom: 0; }
	}
</style>
