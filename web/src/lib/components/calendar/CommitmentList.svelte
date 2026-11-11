<script lang="ts">
	import Button from '$lib/components/common/Button.svelte';
	import Chip from '$lib/components/common/Chip.svelte';
	import { canMove, entryHref, entryKey, isDateOnly, roleDay, roleText, todayIn, wallToInstant, type AgendaAction } from '$lib/calendar/presentation';
	import type { AgendaEntry, AgendaResponse, AgendaWorkEntry } from '$lib/types';
	let { data, zone, pending, saved, onaction }: {
		data: AgendaResponse; zone: string; pending: Record<string, boolean>; saved: Record<string, boolean>;
		onaction: (entry: AgendaEntry, action: AgendaAction) => void;
	} = $props();
	const id = $props.id();
	const ranks = ['high', 'normal', 'low', 'unranked'] as const;
	const reasons: Record<string, string> = { target_passed: 'Target passed', deadline_still_owed: 'Missed deadline · Still owed', waiting: 'Waiting on', blocked_needs_decision: 'Needs a decision' };
	function occurrence(row: AgendaWorkEntry): AgendaEntry | null {
		if (row.type !== 'schedule' || !row.roles?.target) return null;
		const target = row.roles.target;
		const at = isDateOnly(target) ? wallToInstant(`${target}T00:00`, zone) : target;
		return { ...row, type: 'schedule', state: 'outstanding', at, day: roleDay(target, zone), all_day: row.all_day ?? isDateOnly(target) };
	}
</script>

{#snippet rows(entries: AgendaWorkEntry[])}
	<ul>
		{#each entries as row, index (index)}
			{@const entry = occurrence(row)}
			{@const href = entryHref({ ...row, type: 'schedule', at: row.at ?? '', day: row.day ?? '' })}
			{@const busy = entry && (pending[entryKey(entry)] || saved[entryKey(entry)])}
			<li>
				{#if href}<a {href}>{row.item_title}</a>{:else}<span>{row.item_title}</span>{/if}
				<div class="meta">{row.workspace_name} / {row.collection} / {row.item_ref}</div>
				{#if reasons[row.reason]}<Chip size="sm">{reasons[row.reason]}</Chip>{/if}
				{#if row.roles?.target}<div class="date">Target {roleText(row.roles.target, zone)}</div>{/if}
				{#if row.roles?.deadline}<div class="date">Deadline {roleText(row.roles.deadline, zone)}</div>{/if}
				{#if entry && canMove(entry)}
					<div class="actions">
						<Button size="sm" disabled={!!busy} onclick={() => onaction(entry, 'move')}>Move target…</Button>
						{#if entry.day !== todayIn(zone)}<Button size="sm" disabled={!!busy} onclick={() => onaction(entry, 'today')}>Bring to today</Button>{/if}
					</div>
					{#if pending[entryKey(entry)]}<span>Saving…</span>{/if}
				{/if}
			</li>
		{/each}
	</ul>
{/snippet}

<section aria-labelledby={`${id}-priority`}>
	<h2 id={`${id}-priority`}>Priority</h2>
	<p>Showing all outstanding</p>
	{#each ranks as rank, index (index)}
		{@const ranked = data.priority_list.filter(row => (row.rank ?? 'unranked') === rank)}
		{#if ranked.length}
			<h3>{rank[0].toUpperCase() + rank.slice(1)}</h3>
			{@render rows(ranked)}
		{/if}
	{/each}
	{#if !data.priority_list_complete}<p>This list is incomplete.</p>
	{:else if !data.priority_list.length}<p>Nothing outstanding.</p>{/if}
	<h3>Waiting on</h3>
	{@render rows(data.waiting_on)}
	{#if !data.waiting_on_complete}<p>This list is incomplete.</p>
	{:else if !data.waiting_on.length}<p>Nothing waiting.</p>{/if}
	<h3>No date</h3>
	{@render rows(data.no_date)}
	{#if !data.no_date_complete}<p>This list is incomplete.</p>
	{:else if !data.no_date.length}<p>No undated work.</p>{/if}
</section>

<style>
	section { min-width: 0; border: 1px solid var(--border); border-radius: var(--radius); padding: var(--space-4); }
	h2 { font-size: 1.1rem; margin: 0; }
	h3 { font-size: 0.95rem; margin: var(--space-4) 0 var(--space-2); }
	p, .meta, .date { color: var(--text-secondary); font-size: 0.85rem; }
	ul { list-style: none; padding: 0; margin: 0; }
	li { border-bottom: 1px solid var(--border); padding: var(--space-3) 0; overflow-wrap: anywhere; }
	li > a { color: var(--text-primary); font-weight: 600; text-decoration: none; }
	li > a:hover { text-decoration: underline; }
	.meta, .date, .actions { margin-top: var(--space-1); }
	.actions { display: flex; flex-wrap: wrap; gap: var(--space-1); }
</style>
