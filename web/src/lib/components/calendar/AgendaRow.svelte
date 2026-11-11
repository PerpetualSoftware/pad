<script lang="ts">
	import Chip from '$lib/components/common/Chip.svelte';
	import OccurrenceActions from './OccurrenceActions.svelte';
	import { canResolve, canMove, roleText, entryChips, entryHref, isAllDay, isDateOnly, localTime, type AgendaAction } from '$lib/calendar/presentation';
	import type { AgendaEntry } from '$lib/types';

	let { entry, zone, pending = false, saved = false, week = false, onaction, ondragstart, ondragend }: {
		entry: AgendaEntry; zone: string; pending?: boolean; saved?: boolean; week?: boolean;
		onaction: (entry: AgendaEntry, action: AgendaAction) => void;
		ondragstart?: (entry: AgendaEntry, event: DragEvent) => void; ondragend?: () => void;
	} = $props();
	const href = $derived(entryHref(entry));
	const done = $derived(entry.type === 'schedule' && entry.state === 'done');
</script>

<li class="agenda-row" class:week data-ref={entry.item_ref}
	draggable={week && canMove(entry) && !pending && !saved}
	ondragstart={event => ondragstart?.(entry, event)} ondragend={ondragend}>
	<div class="time">{#if week && entry.type === 'schedule'}Target {/if}{isAllDay(entry) ? 'All day' : localTime(entry.at, zone)}</div>
	<div class="main">
		<div class="title" class:done>
			{#if done}<span aria-label="Done">✓</span>{/if}
			{#if href}<a {href}>{entry.item_title}</a>{:else}<span>{entry.item_title}</span>{/if}
		</div>
		<div class="meta"><span>{entry.workspace_name} / {entry.collection}</span><span>{entry.item_ref}</span></div>
		{#if week && entry.type === 'schedule'}
			{#if entry.roles?.earliest}<div class="role">Available from {roleText(entry.roles.earliest, zone)}</div>{/if}
			{#if entry.roles?.deadline}<div class="role">Deadline {roleText(entry.roles.deadline, zone)}{entry.roles.deadline_behavior === 'stays_owed' ? ' · Still owed' : entry.roles.deadline_behavior === 'lapses' ? ' · Lapses' : ''}</div>{/if}
			{#if entry.roles?.occupied_until}<div class="role">Until {isDateOnly(entry.roles.occupied_until) ? roleText(entry.roles.occupied_until, zone) : localTime(entry.roles.occupied_until, zone)}</div>{/if}
		{/if}
	</div>
	<div class="chips">
		{#each entryChips(entry) as chip, index (index)}
			<Chip size="sm" color={chip.color} dot><span class="chip-text">{chip.text}</span></Chip>
		{/each}
	</div>
	{#if canResolve(entry)}
		<OccurrenceActions {entry} {zone} {pending} disabled={saved} {onaction} />
	{/if}
</li>

<style>
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
	.role { margin-top: var(--space-1); color: var(--text-secondary); font-size: 0.8rem; }
	.week { flex-direction: column; gap: var(--space-2); padding: var(--space-3); }
	.week .time { flex: none; }
	.week .chips { max-width: 100%; }
	.week[draggable="true"] { cursor: grab; }
</style>
