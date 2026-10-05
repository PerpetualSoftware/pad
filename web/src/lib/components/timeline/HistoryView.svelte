<script lang="ts">
	/**
	 * The History tab (PLAN-2348 U3): the item's changes and versions as ONE
	 * feed of events — who did what, through which door, when — replacing the
	 * separate Activity and Versions tabs.
	 *
	 * PRESENTATION ONLY, like TimelineEntryList: the rows come grouped from the
	 * owner's one feed (`groupHistory`), and fetching, pagination and the SSE
	 * subscription stay in ItemTimeline. The body sections are
	 * TimelineVersionCard, which carries its own restore flow and fences.
	 */
	import type { Item } from '$lib/types';
	import type { ChangeContext } from '$lib/timeline/changeContext';
	import { statusColor, priorityColor } from '$lib/utils/fieldColors';
	import Chip from '$lib/components/common/Chip.svelte';
	import { IMPORTED_TITLE } from '$lib/utils/imported';
	import { viaAppTitle } from '$lib/utils/viaApp';
	import ActivityChangeValue from './ActivityChangeValue.svelte';
	import TimelineVersionCard from './TimelineVersionCard.svelte';
	import {
		eventVerb,
		whoName,
		sourceLabel,
		matchesFilter,
		peopleOf,
		dayLabel,
		type HistoryRow,
		type HistoryEvent,
		type HistoryFilter
	} from './historyEvents';

	interface Props {
		rows: HistoryRow[];
		/** The item's kind in the sentence: "created this task". */
		itemNoun?: string;
		/** Render the empty line (computed by the owner over the whole feed). */
		showEmpty?: boolean;
		changeContext?: ChangeContext;
		wsSlug: string;
		itemSlug: string;
		currentContent: string;
		currentContentStale?: boolean;
		onRestore?: (item: Item) => void;
		flushBeforeRestore?: () => Promise<void>;
		restoreFrozen?: boolean;
	}

	let {
		rows,
		itemNoun = 'item',
		showEmpty = false,
		changeContext,
		wsSlug,
		itemSlug,
		currentContent,
		currentContentStale = false,
		onRestore,
		flushBeforeRestore,
		restoreFrozen = false
	}: Props = $props();

	const FILTERS: Array<{ id: HistoryFilter; label: string }> = [
		{ id: 'all', label: 'All' },
		{ id: 'fields', label: 'Fields' },
		{ id: 'content', label: 'Content' },
		{ id: 'notes', label: 'Notes & decisions' }
	];

	let filter = $state<HistoryFilter>('all');
	let person = $state<string | null>(null);
	let openAutosaves = $state<Record<string, boolean>>({});

	const people = $derived(peopleOf(rows));
	// A chosen person who has left the feed (an item switch) is no filter.
	const activePerson = $derived(person !== null && people.includes(person) ? person : null);
	const shown = $derived(rows.filter((r) => matchesFilter(r, filter, activePerson)));

	function fieldLabel(key: string): string {
		return (changeContext?.fieldFor(key)?.label || key.replace(/_/g, ' ')).toLowerCase();
	}

	function titleCase(s: string): string {
		return s.charAt(0).toUpperCase() + s.slice(1);
	}

	function timeOf(at: string): string {
		return new Date(at).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
	}

	function chipColor(field: string, value: string): string | undefined {
		if (field === 'status') return statusColor(value);
		if (field === 'priority') return priorityColor(value);
		return undefined;
	}

	function initial(name: string): string {
		return (name.trim().charAt(0) || '?').toUpperCase();
	}

	function avatarClass(row: HistoryRow): string {
		if (row.who.kind === 'agent') return 'av-agent';
		if (row.who.kind === 'system') return 'av-system';
		return 'av-user';
	}

	/**
	 * The newest saved version in the feed, and the index of its row. A
	 * throttled edit (`bodyEdited`) at or after that row is part of the
	 * current body's difference from it, so it can show that diff; an older
	 * one sits between two saved versions and cannot be isolated.
	 */
	const newestVersion = $derived.by(() => {
		for (let i = 0; i < rows.length; i++) {
			const r = rows[i];
			const v = r.type === 'autosave' ? r.entry.version : r.versions[0]?.version;
			if (v) return { index: i, version: v };
		}
		return null;
	});

	function movedMeta(ev: HistoryEvent): { from?: string; to?: string; dropped?: string; notUnique?: string } | null {
		const moved = ev.actions.find((a) => a.action === 'moved');
		if (!moved) return null;
		try {
			const m = JSON.parse(moved.metadata);
			return { from: m.from_collection, to: m.to_collection, dropped: m.dropped_fields, notUnique: m.not_unique };
		} catch {
			return {};
		}
	}
</script>

<div class="history">
	<div class="filters" role="toolbar" aria-label="Filter history">
		{#each FILTERS as f (f.id)}
			<button
				type="button"
				class="filter-chip"
				class:on={filter === f.id}
				aria-pressed={filter === f.id}
				onclick={() => (filter = f.id)}>{f.label}</button
			>
		{/each}
		{#if people.length > 1}
			<label class="filter-chip people" class:on={activePerson !== null}>
				<span class="sr-only">Person</span>
				<select
					value={activePerson ?? ''}
					onchange={(e) => (person = (e.currentTarget as HTMLSelectElement).value || null)}
				>
					<option value="">People</option>
					{#each people as p (p)}
						<option value={p}>{p}</option>
					{/each}
				</select>
			</label>
		{/if}
	</div>

	{#each shown as row, i (row.id)}
		{@const day = dayLabel(row.at)}
		{#if i === 0 || dayLabel(shown[i - 1].at) !== day}
			<div class="day"><span>{day}</span></div>
		{/if}
		<div class="row" data-testid="history-row" data-row-type={row.type}>
			<div class="rail">
				<span class="avatar {avatarClass(row)}" aria-hidden="true">{initial(whoName(row.who))}</span>
				<span class="line"></span>
			</div>

			{#if row.type === 'autosave'}
				{@const run = row.entry.autosave_run}
				{@const added = run ? run.lines_added : row.entry.version?.lines_added}
				{@const removed = run ? run.lines_removed : row.entry.version?.lines_removed}
				<div class="autosave">
					<button
						type="button"
						class="autosave-toggle"
						aria-expanded={!!openAutosaves[row.id]}
						onclick={() => (openAutosaves[row.id] = !openAutosaves[row.id])}
					>
						<span class="caret" class:open={openAutosaves[row.id]} aria-hidden="true">▸</span>
						<span>
							{row.count === 1 ? 'Autosaved' : `${row.count} autosaves`} by <bdi>{whoName(row.who)}</bdi> while editing
						</span>
						{#if row.who.viaAppName}
							<span class="via-app" title={viaAppTitle(row.who.viaAppName)}>via <bdi>{row.who.viaAppName}</bdi></span>
						{/if}
						{#if row.who.imported}
							<span class="imported" title={IMPORTED_TITLE}><Chip size="sm">Imported</Chip></span>
						{/if}
						<span class="muted">·</span>
						<span class="muted" title={new Date(row.at).toLocaleString()}
							>{row.count > 1 ? `${timeOf(row.firstAt)}–${timeOf(row.at)}` : timeOf(row.at)}</span
						>
						{#if added !== undefined && removed !== undefined}
							<span class="muted">·</span>
							<span><span class="added">+{added}</span> <span class="removed">−{removed}</span> lines</span>
						{/if}
					</button>
					{#if openAutosaves[row.id] && row.entry.version}
						<div class="autosave-body">
							<TimelineVersionCard
								version={row.entry.version}
								run={run}
								{wsSlug}
								{itemSlug}
								{currentContent}
								{currentContentStale}
								{onRestore}
								{flushBeforeRestore}
								frozen={restoreFrozen}
							/>
						</div>
					{/if}
				</div>
			{:else}
				{@const ev = row}
				{@const moved = movedMeta(ev)}
				<article class="card">
					<header class="head">
						<bdi class="name">{whoName(ev.who)}</bdi>
						{#if ev.who.kind === 'agent'}
							<Chip size="sm" color="var(--accent-purple)">Agent</Chip>
						{/if}
						{#if ev.who.viaAppName}
							<span class="via-app" title={viaAppTitle(ev.who.viaAppName)}>via <bdi>{ev.who.viaAppName}</bdi></span>
						{/if}
						{#if ev.who.imported}
							<span class="imported" title={IMPORTED_TITLE}><Chip size="sm">Imported</Chip></span>
						{/if}
						{#if ev.who.kind === 'system'}
							<Chip size="sm">Recovered</Chip>
						{:else if sourceLabel(ev.who.source)}
							<Chip size="sm">{sourceLabel(ev.who.source)}</Chip>
						{/if}
						<span class="verb">
							{#if ev.who.kind === 'agent' && ev.who.user}for <bdi>{ev.who.user}</bdi>{' · '}{/if}{eventVerb(
								ev,
								itemNoun,
								fieldLabel
							)}
						</span>
						<span class="spacer"></span>
						<time class="time" datetime={ev.at} title={new Date(ev.at).toLocaleString()}>{timeOf(ev.at)}</time>
					</header>

					{#if moved && (moved.from || moved.to)}
						<div class="field-row">
							<span class="field-label">Collection</span>
							<span class="values">{moved.from} <span class="arrow">→</span> {moved.to}</span>
						</div>
					{/if}
					{#if moved?.dropped}
						<div class="field-row">
							<span class="field-label">Dropped on move</span>
							<span class="values">{moved.dropped}</span>
						</div>
					{/if}
					{#if moved?.notUnique}
						<div class="field-row">
							<span class="field-label">Not unique</span>
							<span class="values">{moved.notUnique}</span>
						</div>
					{/if}

					<!-- A create row carries no field changes of its own (the created
					     activity records none), so any change here came from an update
					     folded into the same event, and reads as from → to like any other. -->
					{#each ev.changes as c (c.field)}
						{@const fromColor = chipColor(c.field, c.from)}
						{@const toColor = chipColor(c.field, c.to)}
						<div class="field-row" data-field={c.field}>
							<span class="field-label">{titleCase(fieldLabel(c.field))}</span>
							<span class="values">
								<s class="from">
									{#if fromColor}<Chip size="sm" color={fromColor}>{c.from}</Chip>{:else}<ActivityChangeValue
											text={c.from}
											field={changeContext?.fieldFor(c.field)}
											context={changeContext}
										/>{/if}
								</s>
								<span class="arrow" aria-label="to">→</span>
								{#if toColor}<Chip size="sm" color={toColor}>{c.to}</Chip>{:else}<span class="to"
										><ActivityChangeValue text={c.to} field={changeContext?.fieldFor(c.field)} context={changeContext} /></span
									>{/if}
							</span>
						</div>
					{/each}

					{#each ev.versions as v (v.id)}
						{#if v.version}
							<TimelineVersionCard
								version={v.version}
								{wsSlug}
								{itemSlug}
								{currentContent}
								{currentContentStale}
								{onRestore}
								{flushBeforeRestore}
								frozen={restoreFrozen}
							/>
						{/if}
					{/each}
					{#if ev.bodyEdited}
						{#if newestVersion && newestVersion.index >= rows.indexOf(ev)}
							<TimelineVersionCard
								version={newestVersion.version}
								sinceNow
								{wsSlug}
								{itemSlug}
								{currentContent}
								{currentContentStale}
								frozen={restoreFrozen}
							/>
						{:else}
							<div class="field-row">
								<span class="field-label">Description</span>
								<span class="values muted">changed (no version was saved for this edit)</span>
							</div>
						{/if}
					{/if}

					{#each ev.notes as n (n.id)}
						<div class="structured note">
							<span class="tag">Note</span>
							<span class="summary">{n.note?.summary ?? ''}</span>
							{#if n.note?.details}<span class="detail">{n.note.details}</span>{/if}
						</div>
					{/each}
					{#each ev.decisions as d (d.id)}
						<div class="structured decision">
							<span class="tag">Decision</span>
							<span class="summary">{d.decision?.decision ?? ''}</span>
							{#if d.decision?.rationale}<span class="detail">{d.decision.rationale}</span>{/if}
						</div>
					{/each}
				</article>
			{/if}
		</div>
	{/each}

	{#if showEmpty}
		<div class="empty">No history yet.</div>
	{:else if rows.length > 0 && shown.length === 0}
		<div class="empty">Nothing matches this filter.</div>
	{/if}
</div>

<style>
	.history {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
		min-width: 0;
	}

	.filters {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: var(--space-2);
	}

	.filter-chip {
		display: inline-flex;
		align-items: center;
		padding: 0.3em 0.9em;
		border: 1px solid var(--border);
		border-radius: 999px;
		background: var(--bg-secondary);
		color: var(--text-secondary);
		font: inherit;
		font-size: 0.85em;
		cursor: pointer;
	}

	.filter-chip:hover {
		background: var(--bg-tertiary);
	}

	.filter-chip.on {
		border-color: var(--accent-purple);
		color: var(--accent-purple);
		background: color-mix(in srgb, var(--accent-purple) 12%, transparent);
		font-weight: 600;
	}

	.people select {
		appearance: none;
		border: none;
		background: transparent;
		color: inherit;
		font: inherit;
		line-height: inherit;
		height: auto;
		min-height: 0;
		margin: 0;
		padding: 0 0.2em 0 0;
		cursor: pointer;
	}

	.people::after {
		content: '▾';
		font-size: 0.8em;
		pointer-events: none;
	}

	.sr-only {
		position: absolute;
		width: 1px;
		height: 1px;
		overflow: hidden;
		clip: rect(0 0 0 0);
		white-space: nowrap;
	}

	.day {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		font-size: 0.75em;
		letter-spacing: 0.06em;
		text-transform: uppercase;
		color: var(--text-muted);
	}

	.day::after {
		content: '';
		flex: 1;
		border-top: 1px solid var(--border);
	}

	.row {
		display: flex;
		gap: var(--space-3);
		min-width: 0;
	}

	.rail {
		display: flex;
		flex-direction: column;
		align-items: center;
		flex-shrink: 0;
	}

	.avatar {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		width: 28px;
		height: 28px;
		border-radius: 50%;
		font-size: 0.8em;
		font-weight: 600;
		color: #fff;
	}

	.av-user {
		background: var(--status-blue);
	}

	.av-agent {
		background: var(--accent-purple);
	}

	.av-system {
		background: var(--text-muted);
	}

	.line {
		flex: 1;
		width: 2px;
		margin-top: var(--space-1);
		background: var(--border);
	}

	.row:last-of-type .line {
		display: none;
	}

	.card {
		flex: 1;
		min-width: 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		padding: var(--space-2) var(--space-3);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
	}

	.head {
		display: flex;
		align-items: center;
		flex-wrap: wrap;
		gap: var(--space-2);
		min-width: 0;
	}

	.name {
		font-weight: 600;
		max-width: 24ch;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.verb {
		color: var(--text-secondary);
		min-width: 0;
		overflow-wrap: anywhere;
	}

	.spacer {
		flex: 1;
	}

	.time {
		font-size: 0.8em;
		color: var(--text-muted);
		white-space: nowrap;
	}

	.field-row {
		display: flex;
		align-items: baseline;
		gap: var(--space-3);
		flex-wrap: wrap;
		font-size: 0.85em;
	}

	.field-label {
		color: var(--text-muted);
		min-width: 6.5em;
	}

	.values {
		display: inline-flex;
		align-items: center;
		flex-wrap: wrap;
		gap: var(--space-2);
		min-width: 0;
		overflow-wrap: anywhere;
	}

	.from {
		opacity: 0.7;
	}

	.arrow {
		color: var(--text-muted);
	}

	/* The app a write went through (U9c): secondary, never a second author. */
	.via-app {
		font-size: 0.85em;
		color: var(--text-muted);
	}
	.muted {
		color: var(--text-muted);
	}

	.added {
		color: var(--accent-green);
		font-weight: 600;
	}

	.removed {
		color: var(--accent-red);
		font-weight: 600;
	}

	.structured {
		display: flex;
		flex-wrap: wrap;
		align-items: baseline;
		gap: var(--space-2);
		padding: var(--space-2) var(--space-3);
		border-left: 3px solid var(--accent-purple);
		background: var(--bg-tertiary);
		border-radius: 0 var(--radius) var(--radius) 0;
		font-size: 0.85em;
	}

	.structured.decision {
		border-left-color: var(--accent-green);
	}

	.tag {
		font-size: 0.8em;
		font-weight: 700;
		letter-spacing: 0.06em;
		text-transform: uppercase;
		color: var(--accent-purple);
	}

	.decision .tag {
		color: var(--accent-green);
	}

	.summary {
		font-weight: 500;
		overflow-wrap: anywhere;
	}

	.detail {
		color: var(--text-muted);
		overflow-wrap: anywhere;
	}

	.autosave {
		flex: 1;
		min-width: 0;
		border: 1px dashed var(--border);
		border-radius: var(--radius);
	}

	.autosave-toggle {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: var(--space-2);
		width: 100%;
		padding: var(--space-2) var(--space-3);
		background: none;
		border: none;
		font: inherit;
		font-size: 0.85em;
		color: var(--text-secondary);
		text-align: left;
		cursor: pointer;
	}

	.autosave-toggle:hover {
		background: var(--bg-tertiary);
	}

	.caret {
		transition: transform 0.15s ease;
	}

	.caret.open {
		transform: rotate(90deg);
	}

	.autosave-body {
		padding: 0 var(--space-3) var(--space-3);
	}

	.empty {
		padding: var(--space-3);
		font-size: 0.85em;
		color: var(--text-muted);
		text-align: center;
	}
</style>
