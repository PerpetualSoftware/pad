<script lang="ts">
	import { deadlinePill, forecastPill } from '$lib/calendar/presentation';
	import type { CommitmentRoles } from '$lib/types';

	interface Props {
		kind: 'deadline' | 'forecast';
		/** The role value: a date-only `YYYY-MM-DD` or an instant. */
		value: string;
		/** The viewer's time zone. */
		zone: string;
		/** `deadline` only: what happens when it passes. */
		behavior?: CommitmentRoles['deadline_behavior'];
		/** `forecast` only: the deadline it is measured against. */
		deadline?: string | null;
		/** The moment tone and wording are judged from. */
		now?: Date;
		/** Extra detail for the tooltip, such as who set a forecast. */
		title?: string;
	}

	let { kind, value, zone, behavior = null, deadline = null, now = new Date(), title }: Props = $props();

	const pill = $derived.by(() => {
		if (kind === 'deadline') return deadlinePill(value, behavior, zone, now);
		const forecast = forecastPill(value, deadline, zone, now);
		return { tone: forecast.late ? 'late' : 'ontime', text: forecast.text };
	});
	// The icon is decorative, so the role is spoken unless the wording already opens with it.
	const prefix = $derived(kind === 'forecast' ? 'Forecast' : pill.tone === 'passed' ? '' : 'Deadline');
</script>

<span class="pill" data-kind={kind} data-tone={pill.tone} {title}>
	{#if kind === 'deadline'}
		<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M5 21V4h11l-2 4 2 4H5" /></svg>
	{:else}
		<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 9c3-3 5 3 8 0s5 3 8 0" /><path d="M4 15c3-3 5 3 8 0s5 3 8 0" /></svg>
	{/if}
	{#if prefix}<span class="sr-only">{prefix}{' '}</span>{/if}<span>{pill.text}</span>
</span>

<style>
	.pill {
		--pill-c: var(--accent-gray);
		display: inline-flex;
		align-items: center;
		gap: 4px;
		padding: 0 7px;
		border: 1px solid transparent;
		border-radius: 6px;
		font-size: 11.5px;
		font-weight: 500;
		line-height: 1.6;
		white-space: nowrap;
		color: color-mix(in srgb, var(--pill-c) var(--chip-text-mix, 100%), #000);
	}

	svg {
		width: 13px;
		height: 13px;
		flex: 0 0 13px;
	}

	[data-kind='deadline'] {
		background: color-mix(in srgb, var(--pill-c) var(--chip-alpha, 16%), transparent);
	}

	[data-tone='soon'] {
		--pill-c: var(--accent-amber);
	}

	[data-tone='passed'],
	[data-tone='late'] {
		--pill-c: var(--accent-red);
	}

	/* A forecast is an estimate, so it is outlined and dashed rather than filled. */
	[data-kind='forecast'] {
		border: 1px dashed var(--border-strong);
		color: var(--text-secondary);
	}

	[data-kind='forecast'][data-tone='late'] {
		border-color: color-mix(in srgb, var(--accent-red) 60%, transparent);
		color: color-mix(in srgb, var(--accent-red) var(--chip-text-mix, 100%), #000);
	}
</style>
