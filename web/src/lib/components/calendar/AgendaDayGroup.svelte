<script lang="ts">
	import Chip from '$lib/components/common/Chip.svelte';
	import AgendaEvents from './AgendaEvents.svelte';
	import AgendaRow from './AgendaRow.svelte';
	import { entryKey, isAllDay, isEvent, localTime, longDay, roleDay, shortDay, type AgendaAction } from '$lib/calendar/presentation';
	import type { AgendaEntry, CommitmentMoveResult } from '$lib/types';

	interface Props {
		day: string;
		today: string;
		/** The day's rows, in display order. Created and completed entries fold into one line. */
		entries: AgendaEntry[];
		/** One line beside the heading, such as "2 owed · 1 done". */
		summary?: string;
		/** How many of the day's entries are listed under Needs attention instead of here. */
		lifted?: number;
		/** Occurrences whose deadline lands here while their target is on another day. */
		deadlines?: AgendaEntry[];
		/** Occurrences whose forecast lands here while their target is on another day. */
		forecasts?: AgendaEntry[];
		zone: string;
		now?: Date;
		/** False while the day may have entries that did not load: an empty day then says nothing. */
		complete?: boolean;
		pending: Record<string, boolean>;
		saved: Record<string, boolean>;
		compact?: boolean;
		/** Rows may be dragged to another day. */
		movable?: boolean;
		heading?: boolean;
		/** The occurrence being dragged, the day it is over, and what the move would change. */
		dragging?: AgendaEntry | null;
		hover?: boolean;
		impact?: CommitmentMoveResult | null;
		titles?: Record<string, string>;
		onaction: (entry: AgendaEntry, action: AgendaAction) => void;
		ondragstart?: (entry: AgendaEntry, event: DragEvent) => void;
		ondragend?: () => void;
		onenter?: (day: string) => void;
		ondrop?: (day: string) => void;
	}

	let { day, today, entries, summary = '', lifted = 0, deadlines = [], forecasts = [], zone, now = new Date(Date.now()), complete = true, pending, saved, compact = false, movable = false, heading = true,
		dragging = null, hover = false, impact = null, titles = {}, onaction, ondragstart, ondragend, onenter, ondrop }: Props = $props();

	const uid = $props.id();
	const rows = $derived(entries.filter(entry => !isEvent(entry)));
	const events = $derived(entries.filter(isEvent));
	const linked = $derived(impact?.changes.filter(change => !change.root && roleDay(change.to, zone) === day) ?? []);
	const landing = $derived(hover && dragging && dragging.day !== day ? dragging : null);
	const empty = $derived(rows.length + events.length + deadlines.length + forecasts.length === 0);
</script>

<section class="day" class:drop={hover && !!dragging} data-day={day} aria-labelledby={heading ? `${uid}-h` : undefined} aria-label={heading ? undefined : longDay(day)}
	ondragenter={event => { if (dragging) { event.preventDefault(); onenter?.(day); } }}
	ondragover={event => { if (dragging) { event.preventDefault(); if (event.dataTransfer) event.dataTransfer.dropEffect = 'move'; } }}
	ondrop={event => { event.preventDefault(); ondrop?.(day); }}>
	{#if heading}
		<div class="head">
			<h2 id={`${uid}-h`} tabindex="-1" data-day-heading={day}>{longDay(day)}</h2>
			{#if day === today}<span class="today">Today</span>{/if}
			{#if summary}<span class="sum">{summary}</span>{/if}
		</div>
	{/if}
	<ul class="list">
		{#each rows as entry (entryKey(entry))}
			<AgendaRow {entry} {zone} {now} {compact} {movable} dragging={!!dragging && entryKey(dragging) === entryKey(entry)}
				pending={pending[entryKey(entry)]} saved={saved[entryKey(entry)]} {onaction} {ondragstart} {ondragend} />
		{/each}
		{#each deadlines as entry, index (index)}<AgendaRow {entry} {zone} {now} {compact} marker="deadline" {onaction} />{/each}
		{#each forecasts as entry, index (index)}<AgendaRow {entry} {zone} {now} {compact} marker="forecast" {onaction} />{/each}
		{#if landing}
			<li class="ghost" aria-hidden="true">
				<span class="time">{isAllDay(landing) ? 'All day' : localTime(landing.at, zone)}</span>
				<span class="what">{landing.item_title}</span>
				<Chip size="sm" color="var(--accent-primary)">Moves here</Chip>
			</li>
		{/if}
		{#each linked as change, index (index)}
			<li class="ghost" aria-hidden="true">
				<span class="time">{change.from ? `${shortDay(roleDay(change.from, zone), today)} →` : ''}</span>
				<span class="what">{titles[change.item_id] ?? 'Linked item'} (linked) · proposed</span>
				{#if impact?.violations?.some(violation => violation.item_id === change.item_id && violation.role === change.role)}<Chip size="sm" color="var(--accent-red)">Outside its bound</Chip>{/if}
			</li>
		{/each}
		<AgendaEvents {events} {zone} {now} {compact} {onaction} />
		{#if empty && complete && !landing && !linked.length}<li class="empty">{lifted > 0 ? 'Listed under Needs attention.' : 'Nothing scheduled.'}</li>{/if}
	</ul>
</section>

<style>
	.day { margin-bottom: var(--space-5); min-width: 0; }
	.head { display: flex; flex-wrap: wrap; align-items: baseline; gap: 0 var(--space-2); padding: 0 var(--space-1) var(--space-2); }
	h2 { margin: 0; font-size: 14px; font-weight: 700; scroll-margin-top: var(--space-4); }
	h2:focus-visible { outline: 2px solid var(--accent-primary); outline-offset: 2px; border-radius: var(--radius-sm); }
	.today { font-size: 11px; font-weight: 700; color: var(--text-on-accent); background: var(--accent-primary-strong); border-radius: 99px; padding: 0 8px; }
	.sum { margin-left: auto; font-size: 12px; color: var(--text-muted); }
	.list { list-style: none; margin: 0; padding: 0; }
	.list { overflow: hidden; background: var(--card-bg); border: 1px solid var(--card-border); border-radius: var(--radius-lg); box-shadow: var(--shadow-card); }
	.list > :global(* + *) { border-top: 1px solid var(--border-subtle); }
	.drop .list { outline: 2px dashed var(--accent-primary); outline-offset: 2px; }
	.empty { padding: 10px var(--space-4); color: var(--text-muted); font-size: 13px; }
	.ghost { display: flex; align-items: center; gap: var(--space-3); min-height: 34px; margin: 4px; padding: 5px var(--space-3); border: 1px dashed var(--accent-primary); border-radius: var(--radius-sm); background: color-mix(in srgb, var(--accent-primary) 6%, transparent); font-size: 13px; }
	.ghost .time { flex: 0 0 56px; font-size: 12px; color: var(--text-secondary); font-variant-numeric: tabular-nums; white-space: nowrap; }
	.ghost .what { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-weight: 500; }
</style>
