<script lang="ts">
	import AgendaRow from './AgendaRow.svelte';
	import { entryKey, type AgendaAction } from '$lib/calendar/presentation';
	import type { AgendaEntry } from '$lib/types';

	interface Props {
		/** What needs attention, most severe first. Each is listed here instead of in its day. */
		items: { key: string; entry: AgendaEntry }[];
		zone: string;
		now?: Date;
		pending: Record<string, boolean>;
		saved: Record<string, boolean>;
		compact?: boolean;
		onaction: (entry: AgendaEntry, action: AgendaAction) => void;
	}

	let { items, zone, now = new Date(Date.now()), pending, saved, compact = false, onaction }: Props = $props();
</script>

<section class="attention" aria-label="Needs attention">
	<h2>Needs attention</h2><span class="badge">{items.length}</span>
	<ul>
		{#each items as item (item.key)}
			<AgendaRow entry={item.entry} {zone} {now} {compact} showDay pending={pending[entryKey(item.entry)]} saved={saved[entryKey(item.entry)]} {onaction} />
		{/each}
	</ul>
</section>

<style>
	.attention { margin-bottom: var(--space-5); min-width: 0; }
	h2 { display: inline-block; margin: 0 var(--space-2) var(--space-2) 0; font-size: 11.5px; font-weight: 700; letter-spacing: 0.06em; text-transform: uppercase; color: var(--text-secondary); }
	.badge { font-size: 11px; font-weight: 600; color: var(--text-muted); background: var(--bg-tertiary); border-radius: 99px; padding: 0 7px; font-variant-numeric: tabular-nums; }
	ul { list-style: none; margin: 0; padding: 0; overflow: hidden; background: var(--card-bg); border: 1px solid var(--card-border); border-left: 3px solid var(--accent-orange); border-radius: var(--radius-lg); box-shadow: var(--shadow-card); }
	ul > :global(* + *) { border-top: 1px solid var(--border-subtle); }
</style>
