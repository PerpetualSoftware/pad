<script lang="ts">
	import type { AudienceView } from '$lib/calendar/audience';
	import AgendaRow from './AgendaRow.svelte';
	import { entryKey, type AgendaAction } from '$lib/calendar/presentation';
	import type { AgendaEntry } from '$lib/types';

	/** The line that ends a day's list: "N completed ›", folded until asked for. */
	let { audience = 'mine', isAgent = false, completed, zone, now, compact = false, onaction }: {
		audience?: AudienceView;
		isAgent?: boolean;
		/** The day's completed entries. With none, nothing is drawn. */
		completed: AgendaEntry[];
		zone: string; now?: Date; compact?: boolean;
		onaction: (entry: AgendaEntry, action: AgendaAction) => void;
	} = $props();

	const uid = $props.id();
	let open = $state(false);
</script>

{#if completed.length}
	<li class="completed">
		<button type="button" aria-expanded={open} aria-controls={`${uid}-completed`} onclick={() => open = !open}>
			{completed.length} completed
			<svg class:open width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M9 6l6 6-6 6" /></svg>
		</button>
		<ul id={`${uid}-completed`} hidden={!open}>
			{#if open}
				{#each completed as entry (entryKey(entry))}<AgendaRow {audience} {isAgent} {entry} {zone} {now} {compact} {onaction} />{/each}
			{/if}
		</ul>
	</li>
{/if}

<style>
	ul { list-style: none; margin: 0; padding: 0; }
	ul > :global(*) { border-top: 1px solid var(--border-subtle); }
	button { display: flex; align-items: center; gap: var(--space-1); width: 100%; padding: 7px var(--space-4); border: 0; background: transparent; color: var(--text-muted); font: inherit; font-size: 12px; text-align: left; cursor: pointer; }
	button:hover { background: var(--bg-hover); }
	button:focus-visible { outline: 2px solid var(--accent-primary); outline-offset: -2px; }
	svg { transition: transform 0.12s; }
	svg.open { transform: rotate(90deg); }
	@media (prefers-reduced-motion: reduce) { svg { transition: none; } }
	@media (pointer: coarse) { button { min-height: 44px; } }
</style>
