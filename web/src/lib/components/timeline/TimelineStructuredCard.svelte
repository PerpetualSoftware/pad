<script lang="ts">
	import type { ItemImplementationNote, ItemDecisionLogEntry } from '$lib/types';
	import { relativeTime } from '$lib/utils/markdown';
	import Chip from '$lib/components/common/Chip.svelte';

	// One card for both structured kinds (BUG-2301). They share a shape —
	// headline + optional body + actor + timestamp — and differ only in
	// label, accent and weight, so a variant keeps them from drifting apart
	// the way two near-identical components would.
	let {
		kind,
		note,
		decision,
		actor,
		createdAt
	}: {
		kind: 'note' | 'decision';
		note?: ItemImplementationNote;
		decision?: ItemDecisionLogEntry;
		actor: string;
		createdAt: string;
	} = $props();

	const headline = $derived(kind === 'note' ? (note?.summary ?? '') : (decision?.decision ?? ''));
	const body = $derived(kind === 'note' ? (note?.details ?? '') : (decision?.rationale ?? ''));
	const label = $derived(kind === 'note' ? 'Note' : 'Decision');

	// `actor` is self-declared by whichever client wrote the entry — it lives
	// inside the item's fields blob and the server never stamps it (BUG-2542).
	// Older entries predate that fix and claim "user" regardless of who wrote
	// them, so this labels the claim, not a verified author.
	const actorLabel = $derived(actor === 'agent' ? 'Agent' : actor === 'user' ? 'User' : actor);
</script>

<div class="card" class:decision={kind === 'decision'}>
	<div class="row">
		<Chip size="sm" color={kind === 'decision' ? 'var(--accent-orange)' : 'var(--accent-cyan)'}
			>{label}</Chip
		>
		{#if actorLabel}
			<span class="actor">{actorLabel}</span>
		{/if}
		<span class="spacer"></span>
		<span class="timestamp" title={new Date(createdAt).toLocaleString()}
			>{relativeTime(createdAt)}</span
		>
	</div>
	{#if headline}
		<p class="headline">{headline}</p>
	{/if}
	{#if body}
		<p class="body">{body}</p>
	{/if}
</div>

<style>
	.card {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
		padding: var(--space-2) var(--space-3);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-left: 3px solid var(--accent-cyan);
		border-radius: var(--radius);
	}

	/* A decision log is the thing you go back looking for; it earns more
	   weight than a note, per the BUG-2301 design note. */
	.card.decision {
		border-left-color: var(--accent-orange);
		background: var(--bg-tertiary);
	}

	.row {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		flex-wrap: wrap;
		min-width: 0;
	}

	.actor {
		font-size: 0.85em;
		color: var(--text-muted);
	}

	.spacer {
		flex: 1;
	}

	.timestamp {
		font-size: 0.8em;
		color: var(--text-muted);
		white-space: nowrap;
	}

	.headline {
		margin: 0;
		font-size: 0.9em;
		color: var(--text-primary);
		/* Entries are written as plain text by `pad item note` / `pad item
		   decide` — no markdown pass — so preserve the author's line breaks
		   rather than reflowing them. */
		white-space: pre-wrap;
		overflow-wrap: anywhere;
	}

	.card.decision .headline {
		font-weight: 600;
	}

	.body {
		margin: 0;
		font-size: 0.85em;
		color: var(--text-muted);
		white-space: pre-wrap;
		overflow-wrap: anywhere;
	}
</style>
