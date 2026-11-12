<script lang="ts">
	import AgendaRow from './AgendaRow.svelte';
	import { entryKey, type AgendaAction } from '$lib/calendar/presentation';
	import type { AgendaEntry } from '$lib/types';

	/** The line that ends a day's list: its created and completed entries, folded until asked for. */
	let { events, zone, now, compact = false, onaction }: {
		/** The day's created and completed entries. With none, nothing is drawn. */
		events: AgendaEntry[];
		zone: string; now?: Date; compact?: boolean;
		onaction: (entry: AgendaEntry, action: AgendaAction) => void;
	} = $props();

	const uid = $props.id();
	let open = $state(false);
	const label = $derived((['created', 'completed'] as const)
		.map(type => [events.filter(entry => entry.type === type).length, type] as const)
		.filter(([count]) => count > 0).map(([count, type]) => `${count} ${type}`).join(' · '));
</script>

{#if events.length}
	<li class="events">
		<button type="button" aria-expanded={open} aria-controls={`${uid}-events`} onclick={() => open = !open}>
			<svg class:open width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M9 6l6 6-6 6" /></svg>
			{label}
		</button>
		<ul id={`${uid}-events`} hidden={!open}>
			{#if open}
				{#each events as entry (entryKey(entry))}<AgendaRow {entry} {zone} {now} {compact} {onaction} />{/each}
			{/if}
		</ul>
	</li>
{/if}

<style>
	ul { list-style: none; margin: 0; padding: 0; }
	ul > :global(*) { border-top: 1px solid var(--border-subtle); }
	button { display: flex; align-items: center; gap: var(--space-2); width: 100%; padding: 7px var(--space-4); border: 0; background: transparent; color: var(--text-muted); font: inherit; font-size: 12px; text-align: left; cursor: pointer; }
	button:hover { background: var(--bg-hover); }
	button:focus-visible { outline: 2px solid var(--accent-primary); outline-offset: -2px; }
	svg { transition: transform 0.12s; }
	svg.open { transform: rotate(90deg); }
	@media (prefers-reduced-motion: reduce) { svg { transition: none; } }
	@media (pointer: coarse) { button { min-height: 44px; } }
</style>
