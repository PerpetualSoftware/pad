<script lang="ts">
	import Chip from '$lib/components/common/Chip.svelte';
	import OccurrenceActions from './OccurrenceActions.svelte';
	import RolePill from './RolePill.svelte';
	import StateMark from './StateMark.svelte';
	import WorkspaceTile from './WorkspaceTile.svelte';
	import { OCCURRENCE_STATES, canMove, canResolve, deadlineTone, entryChips, entryHref, forecastSetter, forecastText, isAllDay, isDateOnly, localTime, roleDay, roleText, shortDay, shortRoleDate, todayIn, type AgendaAction } from '$lib/calendar/presentation';
	import type { AgendaEntry } from '$lib/types';

	interface Props {
		entry: AgendaEntry;
		/** The zone dates are read in. Entries carry no zone of their own, so this is the viewer's. */
		zone: string;
		/** The moment pill tones are judged from. The page passes one shared clock. */
		now?: Date;
		pending?: boolean;
		saved?: boolean;
		/** Phone layout: the time and the pills move into the meta line. */
		compact?: boolean;
		/** The row may be dragged to another day, when its occurrence can move at all. */
		movable?: boolean;
		/** This row is the one being dragged. */
		dragging?: boolean;
		/** Lead with the day, for a list that is not grouped by day. */
		showDay?: boolean;
		/** A one-line row for a deadline or forecast that lands on another day than its target. */
		marker?: 'deadline' | 'forecast';
		onaction: (entry: AgendaEntry, action: AgendaAction) => void;
		ondragstart?: (entry: AgendaEntry, event: DragEvent) => void;
		ondragend?: () => void;
	}

	let { entry, zone, now = new Date(Date.now()), pending = false, saved = false, compact = false, movable = false, dragging = false, showDay = false, marker, onaction, ondragstart, ondragend }: Props = $props();

	const href = $derived(entryHref(entry));
	const schedule = $derived(entry.type === 'schedule');
	const live = $derived(schedule && (entry.state === 'outstanding' || entry.state === 'scheduled'));
	const actionable = $derived(canResolve(entry) || (schedule && entry.state === 'lapsed'));
	const density = $derived(marker ? 'marker' : actionable || entry.type === 'date' || (schedule && entry.state === 'missed') ? 'full' : 'quiet');
	const deadline = $derived(live ? entry.roles?.deadline ?? null : null);
	const forecast = $derived(live ? entry.forecast : undefined);
	const deadlinePassed = $derived(!!deadline && deadlineTone(deadline, zone, now) === 'passed');
	// The mark already says outstanding or scheduled, and a passed-deadline pill already says overdue.
	const chips = $derived(entryChips(entry).filter(chip =>
		!(schedule && (chip.text === OCCURRENCE_STATES.outstanding.label || chip.text === OCCURRENCE_STATES.scheduled.label)) && !(deadlinePassed && chip.text === 'Overdue')));
	const draggable = $derived(movable && !marker && canMove(entry) && !pending && !saved);
	const clock = $derived(isAllDay(entry) ? 'All day' : localTime(entry.at, zone));
	const today = $derived(todayIn(zone, now));
	/** A role beside the time: the time alone on the row's own day, else its short date. */
	const beside = (value: string) => !isDateOnly(value) && roleDay(value, zone) === entry.day ? localTime(value, zone) : shortRoleDate(value, zone, now);
	const times = $derived([
		...(showDay ? [shortDay(entry.day, today)] : []), clock,
		...(schedule && entry.roles?.earliest ? [`from ${beside(entry.roles.earliest)}`] : []),
		...(schedule && entry.roles?.occupied_until ? [`until ${beside(entry.roles.occupied_until)}`] : [])
	]);
	const deadlineTitle = $derived(deadline ? `Deadline ${roleText(deadline, zone)}${entry.roles?.deadline_behavior === 'stays_owed' ? ' · Still owed' : entry.roles?.deadline_behavior === 'lapses' ? ' · Lapses' : ''}` : '');
</script>

{#snippet title()}
	{#if href}<a {href}>{entry.item_title}</a>{:else}<span class="untitled">{entry.item_title}</span>{/if}
{/snippet}
{#snippet pills()}
	{#if deadline}<RolePill kind="deadline" value={deadline} behavior={entry.roles?.deadline_behavior} {zone} {now} title={deadlineTitle} />{/if}
	{#if forecast}<RolePill kind="forecast" value={forecast.value} deadline={entry.roles?.deadline} {zone} {now} title={forecastText(forecast, zone)} />{/if}
	<span class="chips">{#each chips as chip, index (index)}<Chip size="sm" color={chip.color}>{chip.text}</Chip>{/each}</span>
{/snippet}

<li class="agenda-row" class:compact class:dragging class:closed={schedule && !live} class:done={schedule && entry.state === 'done'}
	data-ref={entry.item_ref} data-density={density} data-role={marker} {draggable}
	ondragstart={event => ondragstart?.(entry, event)} ondragend={ondragend}>
	{#if marker}
		{#if !compact}<div class="time"></div>{/if}
		<span class="flag" aria-hidden="true">
			<svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
				{#if marker === 'deadline'}<path d="M5 21V4h11l-2 4 2 4H5" />{:else}<path d="M4 9c3-3 5 3 8 0s5 3 8 0" /><path d="M4 15c3-3 5 3 8 0s5 3 8 0" />{/if}
			</svg>
		</span>
		<div class="body">
			<div class="title">
				<span class="kind">{marker === 'deadline' ? 'Deadline' : 'Forecast'}</span>{@render title()}
				<span class="inline-meta">{entry.workspace_name} · {entry.item_ref}{#if marker === 'forecast' && entry.forecast}{' '}· set by {forecastSetter(entry.forecast)}{/if}</span>
			</div>
		</div>
	{:else}
		{#if !compact}<div class="time">{#each times as line, index (index)}{#if index === 0}{line}{:else}<small>{line}</small>{/if}{/each}</div>{/if}
		<span class="mark">
			{#if schedule}<StateMark state={entry.state} overdue={!!entry.overdue || deadlinePassed} />
			{:else if entry.type === 'date'}<StateMark overdue={!!entry.overdue} label={entryChips(entry)[0]?.text || 'Date'} />{/if}
		</span>
		<div class="body">
			<div class="title">
				{@render title()}
				{#if density === 'quiet' && !compact}<span class="inline-meta">{entry.workspace_name} · {entry.item_ref}</span>{/if}
			</div>
			{#if density === 'full' || compact}
				<div class="meta">
					{#if compact}<span class="ptime">{times.join(' ')}</span>{/if}
					{#if entry.workspace_name}<WorkspaceTile name={entry.workspace_name} />{/if}
					<span class="ref">{entry.item_ref}</span>
					{#if entry.audience === 'agent'}<span>· for agents</span>{/if}
					{#if compact}{@render pills()}{/if}
				</div>
			{/if}
		</div>
		<div class="trail">
			{#if !compact}{@render pills()}{/if}
			{#if draggable && !compact}
				<span class="grip" title="Drag to another day" aria-hidden="true">
					<svg width="14" height="14" viewBox="0 0 24 24" fill="currentColor"><circle cx="9" cy="6" r="1.6" /><circle cx="15" cy="6" r="1.6" /><circle cx="9" cy="12" r="1.6" /><circle cx="15" cy="12" r="1.6" /><circle cx="9" cy="18" r="1.6" /><circle cx="15" cy="18" r="1.6" /></svg>
				</span>
			{/if}
			{#if actionable}<OccurrenceActions {entry} {zone} {pending} disabled={saved} {onaction} />{/if}
		</div>
	{/if}
</li>

<style>
	.agenda-row { display: grid; grid-template-columns: 64px 20px minmax(0, 1fr) auto; align-items: center; column-gap: var(--space-3); padding: 9px var(--space-4); min-height: 52px; list-style: none; }
	.agenda-row[draggable='true'] { cursor: grab; }
	.agenda-row.dragging { opacity: 0.45; }
	.time { font-size: 12.5px; color: var(--text-secondary); font-variant-numeric: tabular-nums; white-space: nowrap; }
	.time small { display: block; font-size: 11px; color: var(--text-muted); }
	.mark, .flag { display: grid; place-items: center; }
	.body { min-width: 0; }
	.title { display: flex; align-items: center; gap: 6px; min-width: 0; font-weight: 600; color: var(--text-primary); }
	.title a, .untitled { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: inherit; text-decoration: none; }
	.title a:hover { text-decoration: underline; }
	.title a:focus-visible { outline: 2px solid var(--accent-primary); outline-offset: 2px; border-radius: var(--radius-sm); }
	.meta { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; font-size: 12px; color: var(--text-muted); margin-top: 1px; }
	.ref { font-family: var(--font-mono); font-size: 11.5px; }
	.inline-meta { flex: 0 0 auto; font-size: 12px; font-weight: 400; color: var(--text-muted); white-space: nowrap; }
	.trail { display: flex; align-items: center; justify-content: flex-end; gap: var(--space-2); min-width: 0; }
	.chips { display: contents; }
	.grip { color: var(--text-muted); opacity: 0; }
	.agenda-row:hover .grip, .agenda-row.dragging .grip { opacity: 1; }

	/* Quiet rows: future recurrences and history do not compete with what is owed. */
	[data-density='quiet'] { min-height: 36px; padding-top: 6px; padding-bottom: 6px; }
	[data-density='quiet'] .title { font-weight: 500; color: var(--text-secondary); }
	.closed .title { color: var(--text-muted); }
	.done .title a, .done .untitled { text-decoration: line-through; text-decoration-thickness: 1px; }

	[data-density='marker'] { min-height: 34px; padding-top: 5px; padding-bottom: 5px; background: color-mix(in srgb, var(--bg-tertiary) 55%, transparent); }
	[data-density='marker'] .title { font-weight: 500; font-size: 13px; color: var(--text-secondary); }
	[data-role='deadline'] .flag { color: var(--accent-red); }
	[data-role='forecast'] .flag { color: var(--text-muted); }
	/* A forecast is an estimate: a dashed rule above it, not a filled band. */
	[data-role='forecast'] { background: linear-gradient(to right, var(--border-strong) 50%, transparent 0) top / 8px 1px repeat-x; }

	.compact { grid-template-columns: 18px minmax(0, 1fr) auto; column-gap: 10px; padding: 10px 12px; min-height: 60px; }
	.compact[data-density='quiet'], .compact[data-density='marker'] { min-height: 44px; }
	.compact .title { flex-wrap: wrap; }
	.compact .title a, .compact .untitled { white-space: normal; display: -webkit-box; -webkit-box-orient: vertical; -webkit-line-clamp: 2; line-clamp: 2; }
	.compact .inline-meta { white-space: normal; }
	.compact .trail { gap: 2px; }
	.ptime { color: var(--text-secondary); font-variant-numeric: tabular-nums; font-weight: 600; }
</style>
