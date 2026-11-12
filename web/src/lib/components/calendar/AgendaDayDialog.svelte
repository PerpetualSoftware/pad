<script lang="ts">
	import type { AgendaEntry } from '$lib/types';
	import { dayLabel } from '$lib/calendar/monthGrid';
	import { daySummary, entryKey, type AgendaAction } from '$lib/calendar/presentation';
	import DialogShell from '../common/DialogShell.svelte';
	import AgendaRow from './AgendaRow.svelte';
	let { day, today, entries, zone, now, mobile, incomplete, error = '', pending, saved, onclose, onaction }: {
		day: string | null; today: string;
		/** The day's rows in display order: what needs attention first. */
		entries: AgendaEntry[];
		zone: string; now?: Date; mobile: boolean; incomplete: boolean; error?: string;
		pending: Record<string, boolean>; saved: Record<string, boolean>;
		onclose: () => void; onaction: (entry: AgendaEntry, action: AgendaAction) => void;
	} = $props();
	const title = $derived(day ? `${dayLabel(day, true)}${day === today ? ', Today' : ''}` : '');
	const subject = $derived(entries.length ? [`${entries.length} ${entries.length === 1 ? 'entry' : 'entries'}`, daySummary(entries)].filter(Boolean).join(' · ') : '');
</script>

<DialogShell open={!!day} {mobile} {title} {subject} maxWidth="800px" {onclose}>
	{#if error}<p class="notice bad" role="alert">{error}</p>{/if}
	{#if incomplete}<p class="notice">Not fully loaded</p>{/if}
	{#if !incomplete && entries.length === 0}<p class="none">No entries on this day.</p>{/if}
	{#if day && entries.length}
		<ul>
			{#each entries as entry (entryKey(entry))}
				<AgendaRow {entry} {zone} {now} compact={mobile} pending={pending[entryKey(entry)]} saved={saved[entryKey(entry)]} {onaction} />
			{/each}
		</ul>
	{/if}
</DialogShell>

<style>
	p { margin: 0; font-size: 13px; }
	.none { color: var(--text-secondary); }
	/* The week's notice: amber for what did not load, red for what failed. */
	.notice { padding: 10px var(--space-4); border: 1px solid color-mix(in srgb, var(--accent-amber) 45%, transparent); border-radius: var(--radius); background: color-mix(in srgb, var(--accent-amber) 10%, transparent); }
	.notice.bad { border-color: color-mix(in srgb, var(--accent-red) 35%, transparent); background: color-mix(in srgb, var(--accent-red) 10%, transparent); }
	ul { list-style: none; margin: 0; padding: 0; border: 1px solid var(--card-border); border-radius: var(--radius-lg); background: var(--card-bg); }
	ul > :global(* + *) { border-top: 1px solid var(--border-subtle); }
</style>
