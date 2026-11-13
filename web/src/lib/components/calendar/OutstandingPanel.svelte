<script lang="ts">
	import Chip from '$lib/components/common/Chip.svelte';
	import MenuItem from '$lib/components/common/MenuItem.svelte';
	import SegmentedControl from '$lib/components/common/SegmentedControl.svelte';
	import RolePill from './RolePill.svelte';
	import RowMenu from './RowMenu.svelte';
	import WorkspaceTile from './WorkspaceTile.svelte';
	import { ACKNOWLEDGE_HELP, acknowledgedText, canAcknowledge, canMove, entryHref, entryKey, forecastText, roleDay, roleText, shortDay, shortRoleDate, todayIn, workOccurrence, type AgendaAction } from '$lib/calendar/presentation';
	import type { AgendaEntry, AgendaResponse, AgendaWorkEntry } from '$lib/types';

	interface Props {
		data: AgendaResponse;
		/** The zone dates are read in: the viewer's. */
		zone: string;
		/** The moment pill tones and "today" are judged from. The page passes one shared clock. */
		now?: Date;
		pending: Record<string, boolean>;
		saved: Record<string, boolean>;
		/** Phone layout: taller rows and 44 px menu buttons. */
		compact?: boolean;
		onaction: (entry: AgendaEntry, action: AgendaAction) => void;
	}

	let { data, zone, now = new Date(Date.now()), pending, saved, compact = false, onaction }: Props = $props();

	type Tab = 'priority' | 'waiting' | 'none';
	const id = $props.id();
	const RANKS = [['high', 'High'], ['normal', 'Normal'], ['low', 'Low'], ['unranked', 'Unranked']] as const;
	/** Rows of the By priority tab on show before "Show all". */
	const CUT = 12;
	// Which tab is open lasts as long as the page does; it is not a stored preference.
	let tab = $state<Tab>('priority');
	let all = $state(false);

	const today = $derived(todayIn(zone, now));
	const count = (rows: AgendaWorkEntry[], complete: boolean) => complete ? rows.length : `${rows.length}+`;
	const tabs = $derived([
		{ value: 'priority' as const, label: 'By priority', count: count(data.priority_list, data.priority_list_complete) },
		{ value: 'waiting' as const, label: 'Waiting', count: count(data.waiting_on, data.waiting_on_complete) },
		{ value: 'none' as const, label: 'No date', count: count(data.no_date, data.no_date_complete) }
	].map(option => ({ ...option, id: `${id}-tab-${option.value}`, controls: `${id}-panel` })));

	const rankOf = (row: AgendaWorkEntry) => row.rank ?? 'unranked';
	// Rank order first, server order inside a rank; the cut then falls across ranks.
	const ranked = $derived(RANKS.flatMap(([rank]) => data.priority_list.filter(row => rankOf(row) === rank)));
	const shown = $derived(all ? ranked : ranked.slice(0, CUT));
	const groups = $derived(RANKS.map(([rank, label]) => ({
		label, total: ranked.filter(row => rankOf(row) === rank).length, rows: shown.filter(row => rankOf(row) === rank)
	})).filter(group => group.rows.length));
	const list = $derived(tab === 'waiting'
		? { rows: data.waiting_on, complete: data.waiting_on_complete, empty: 'Nothing waiting.' }
		: tab === 'none'
			? { rows: data.no_date, complete: data.no_date_complete, empty: 'No undated work.' }
			: { rows: data.priority_list, complete: data.priority_list_complete, empty: 'Nothing outstanding.' });
</script>

{#snippet rows(entries: AgendaWorkEntry[])}
	<ul>
		{#each entries as row, index (index)}
			{@const entry = workOccurrence(row, zone)}
			{@const href = entryHref({ ...row, type: 'schedule', at: row.at ?? '', day: row.day ?? '' })}
			{@const key = entry ? entryKey(entry) : ''}
			{@const busy = !!entry && (!!pending[key] || !!saved[key])}
			{@const owed = row.reason === 'deadline_still_owed'}
			{@const deadline = row.roles?.deadline}
			<li data-work={row.item_ref}>
				<div class="text">
					<div class="t">
						{#if href}<a {href} title={row.item_title}>{row.item_title}</a>{:else}<span title={row.item_title}>{row.item_title}</span>{/if}
					</div>
					<div class="m">
						{#if row.workspace_name}<WorkspaceTile name={row.workspace_name} />{/if}
						{#if tab !== 'none'}
							{#if owed}
								<span class="when passed">Deadline passed{#if deadline}{' '}{shortDay(roleDay(deadline, zone), today)}{/if}</span>
							{:else if row.roles?.target}
								<span class="when">{shortRoleDate(row.roles.target, zone, now)}</span>
							{/if}
							<!-- A deadline still owed is already the row's own text, so its pill would only say it twice. -->
							{#if deadline && !owed}<RolePill kind="deadline" value={deadline} behavior={row.roles?.deadline_behavior} {zone} {now} title={`Deadline ${roleText(deadline, zone)}`} />{/if}
							{#if row.forecast}<RolePill kind="forecast" value={row.forecast.value} {deadline} {zone} {now} title={forecastText(row.forecast, zone)} />{/if}
						{/if}
						{#if owed}<Chip size="sm" color="var(--accent-red)">Still owed</Chip>{/if}
						{#if row.missed_count && row.missed_count > 0}<Chip size="sm" color="var(--accent-amber)">{row.missed_count} missed</Chip>{/if}
						{#if row.reason === 'blocked_needs_decision'}<Chip size="sm">Needs a decision</Chip>{/if}
					</div>
					{#if row.state === 'acknowledged'}<div class="ack">{acknowledgedText(row.acknowledged_at, row.acknowledged_by, zone, now)}</div>{/if}
				</div>
				{#if entry && pending[key]}
					<span class="busy"><span class="spinner" aria-hidden="true"></span><span class="sr-only">Saving…</span></span>
				{:else if entry}
					<RowMenu label={`Actions for ${row.item_ref}`} title={row.item_title} disabled={busy}>
						{#snippet children(pick)}
							{#if canAcknowledge(entry)}<MenuItem title={ACKNOWLEDGE_HELP} disabled={busy} onclick={() => pick(() => onaction(entry, 'ack'))}>Acknowledge</MenuItem>{/if}
							<MenuItem disabled={busy} onclick={() => pick(() => onaction(entry, 'forecast'))}>Forecast…</MenuItem>
							{#if canMove(entry)}
								<MenuItem disabled={busy} onclick={() => pick(() => onaction(entry, 'move'))}>Move target…</MenuItem>
								{#if entry.day !== today}<MenuItem disabled={busy} onclick={() => pick(() => onaction(entry, 'today'))}>Bring to today</MenuItem>{/if}
							{/if}
						{/snippet}
					</RowMenu>
				{/if}
			</li>
		{/each}
	</ul>
{/snippet}

<section class="outstanding" class:compact aria-labelledby={`${id}-title`}>
	<div class="head"><h2 id={`${id}-title`}>Outstanding</h2><span class="scope">Your work, any date</span></div>
	<div class="tabs">
		<SegmentedControl mode="tablist" fill label="Outstanding lists" options={tabs} value={tab} onchange={value => tab = value} />
	</div>
	<div id={`${id}-panel`} role="tabpanel" aria-labelledby={`${id}-tab-${tab}`}>
		{#if tab === 'none' && data.no_date_scope === 'personal_only'}<p class="note">Only your own undated work is listed.</p>{/if}
		{#if tab === 'priority'}
			{#each groups as group, index (index)}
				<h3>{group.label} <span class="badge">{group.total}</span></h3>
				{@render rows(group.rows)}
			{/each}
		{:else}
			{@render rows(list.rows)}
		{/if}
		{#if list.complete && !list.rows.length}<p class="note">{list.empty}</p>{/if}
		{#if tab === 'priority' && ranked.length > CUT}
			<p class="foot">
				{#if all}<button type="button" onclick={() => all = false}>Show fewer</button>
				{:else}{ranked.length - CUT} more · <button type="button" onclick={() => all = true}>Show all</button>{/if}
			</p>
		{/if}
		{#if !list.complete}<p class="foot">This list is incomplete.</p>{/if}
	</div>
</section>

<style>
	.outstanding { min-width: 0; }
	.head { display: flex; align-items: center; gap: var(--space-2); padding: var(--space-3) var(--space-4) 0; }
	h2 { margin: 0; font-size: 14px; font-weight: 700; }
	.scope { margin-left: auto; font-size: 11.5px; color: var(--text-muted); }
	.tabs { margin: var(--space-3) var(--space-4) var(--space-2); }
	[role='tabpanel'] { padding-bottom: var(--space-2); }
	h3 { display: flex; gap: 6px; margin: 0; padding: var(--space-2) var(--space-4) 2px; font-size: 11px; font-weight: 700; letter-spacing: 0.05em; text-transform: uppercase; color: var(--text-muted); }
	.badge { font-size: 11px; font-weight: 600; letter-spacing: 0; color: var(--text-muted); background: var(--bg-tertiary); border-radius: 99px; padding: 0 7px; font-variant-numeric: tabular-nums; }
	ul { list-style: none; margin: 0; padding: 0; }
	li { display: grid; grid-template-columns: minmax(0, 1fr) auto; align-items: center; column-gap: var(--space-2); padding: 7px var(--space-4); }
	li + li { border-top: 1px solid var(--border-subtle); }
	.text { min-width: 0; }
	.t { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 13px; font-weight: 600; color: var(--text-primary); }
	.t a { color: inherit; text-decoration: none; }
	.t a:hover { text-decoration: underline; }
	.t a:focus-visible { outline: 2px solid var(--accent-primary); outline-offset: -2px; border-radius: var(--radius-sm); }
	.m { display: flex; flex-wrap: wrap; align-items: center; gap: 6px; margin-top: 1px; font-size: 11.5px; color: var(--text-muted); }
	/* Wraps rather than truncates: who and when are both the point of the line. */
	.ack { margin-top: 1px; font-size: 11.5px; color: var(--text-secondary); }
	.when { color: var(--text-secondary); font-variant-numeric: tabular-nums; }
	.when.passed { color: var(--accent-red); font-weight: 600; }
	.busy { display: grid; place-items: center; width: 32px; height: 32px; }
	.spinner { width: 12px; height: 12px; border: 2px solid var(--border-strong); border-top-color: var(--text-primary); border-radius: 50%; animation: spin 0.8s linear infinite; }
	@keyframes spin { to { transform: rotate(360deg); } }
	@media (prefers-reduced-motion: reduce) { .spinner { animation: none; } }
	.note { margin: 0; padding: var(--space-2) var(--space-4); font-size: 13px; color: var(--text-muted); }
	.foot { margin: 0; padding: var(--space-2) var(--space-4); font-size: 12px; color: var(--text-muted); border-top: 1px solid var(--border-subtle); }
	.foot button { font: inherit; font-weight: 600; color: var(--accent-primary); background: none; border: 0; padding: 0; cursor: pointer; border-radius: var(--radius-sm); }
	.foot button:hover { text-decoration: underline; }
	.foot button:focus-visible { outline: 2px solid var(--accent-primary); outline-offset: 2px; }

	.compact li { padding: 10px 12px; min-height: 56px; }
	.compact .busy, .compact :global(.btn.agenda-row-menu) { width: 44px; height: 44px; }
	.compact .foot button { padding: var(--space-2) 0; }
	@media (pointer: coarse) { .busy { width: 44px; height: 44px; } }
</style>
