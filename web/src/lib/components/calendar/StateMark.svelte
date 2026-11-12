<script lang="ts">
	import { OCCURRENCE_STATES, type OccurrenceState } from '$lib/calendar/presentation';

	interface Props {
		/** The occurrence state. Leave unset for a dated field (due, ends), which draws as a square. */
		state?: OccurrenceState;
		/** An outstanding occurrence whose target or deadline has passed, or an overdue dated field. */
		overdue?: boolean;
		/** Accessible name when the table label does not say it: a dated-field square ("Due"), or a closed item's own status. */
		label?: string;
	}

	let { state, overdue = false, label }: Props = $props();

	const row = $derived(state ? OCCURRENCE_STATES[state] : undefined);
	const shape = $derived(row?.mark ?? 'square');
	const name = $derived(label ?? row?.label ?? 'Date');
	// Colour follows the shape. Red is kept for a lapsed or missed occurrence,
	// an outstanding one past its target or deadline, and an overdue dated field.
	const tone = $derived(
		shape === 'cross' || ((shape === 'ring' || shape === 'square') && overdue) ? 'red'
			: shape === 'ring' ? 'amber'
			: shape === 'check' ? 'green'
			: 'gray'
	);
</script>

<span class="mark" role="img" aria-label={name} title={name} data-mark={shape} data-tone={tone}>
	{#if shape === 'check'}
		<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M5 12.5l4.5 4.5L19 7.5" /></svg>
	{:else if shape === 'dash'}
		<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3" stroke-linecap="round" aria-hidden="true"><path d="M7 12h10" /></svg>
	{:else if shape === 'arrow'}
		<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M5 12h14M13 6l6 6-6 6" /></svg>
	{:else if shape === 'cross'}
		<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3" stroke-linecap="round" aria-hidden="true"><path d="M7 7l10 10M17 7L7 17" /></svg>
	{/if}
</span>

<style>
	.mark {
		display: inline-grid;
		place-items: center;
		box-sizing: border-box;
		width: var(--state-mark-size, 16px);
		height: var(--state-mark-size, 16px);
		flex: 0 0 var(--state-mark-size, 16px);
		border-radius: 50%;
		vertical-align: middle;
	}

	svg {
		width: 68%;
		height: 68%;
	}

	[data-mark='ring'] {
		border: 2px solid var(--accent-amber);
	}

	[data-mark='ring'][data-tone='red'] {
		border-color: var(--accent-red);
		background: color-mix(in srgb, var(--accent-red) 18%, transparent);
	}

	[data-mark='ring-thin'] {
		border: 1.5px solid var(--border-strong);
	}

	[data-mark='ring-dashed'] {
		border: 1.5px dashed var(--border-strong);
	}

	[data-mark='check'] {
		background: var(--accent-green);
		color: var(--text-on-accent);
	}

	[data-mark='dash'],
	[data-mark='arrow'] {
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		color: var(--text-muted);
	}

	[data-mark='cross'] {
		background: color-mix(in srgb, var(--accent-red) 16%, transparent);
		border: 1.5px solid var(--accent-red);
		color: var(--accent-red);
	}

	[data-mark='square'] {
		border: 1.5px solid var(--border-strong);
		border-radius: 4px;
	}

	[data-mark='square'][data-tone='red'] {
		border-color: var(--accent-red);
		background: color-mix(in srgb, var(--accent-red) 18%, transparent);
	}
</style>
