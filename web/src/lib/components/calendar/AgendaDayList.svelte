<script lang="ts">
	import AgendaRow from './AgendaRow.svelte';
	import { dayLabel, entryKey, type AgendaAction } from '$lib/calendar/presentation';
	import type { AgendaEntry } from '$lib/types';

	let { day, entries, zone, pending, saved, onaction, heading = true }: {
		day: string; entries: AgendaEntry[]; zone: string; heading?: boolean;
		pending: Record<string, boolean>; saved: Record<string, boolean>;
		onaction: (entry: AgendaEntry, state: AgendaAction) => void;
	} = $props();
</script>

<section>
	{#if heading}<h2>{dayLabel(day)}</h2>{/if}
	<ul>
		{#each entries as entry (entryKey(entry))}
			<AgendaRow {entry} {zone} pending={pending[entryKey(entry)]} saved={saved[entryKey(entry)]} {onaction} />
		{/each}
	</ul>
</section>

<style>
	h2 { font-size: 1.1rem; margin: var(--space-4) 0 var(--space-2); }
	ul { padding: 0; margin: 0; }
</style>
