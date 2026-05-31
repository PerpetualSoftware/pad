<script lang="ts">
	// Read-only item card for the public board/list renderers (TASK-1679).
	//
	// Strictly presentational: no links, no star, no status cycling, no drag.
	// Renders the title, ref, and a small meta row (status + priority) styled
	// for an anonymous audience. Mirrors the in-app ItemCard's visual language
	// (status/priority colors, uppercase status) without any of its coupling to
	// page.params / mutation handlers.
	//
	// Designed for a future inline read-only expand (TASK-1684): the optional
	// `onactivate` callback + `expandable` flag turn the card into a button-like
	// affordance WITHOUT changing the markup contract. When omitted (the
	// default), the card is an inert <div> — no interactivity is implied.
	import type { FieldDef } from '$lib/types';
	import type { PublicItem } from './shareView';
	import { findField, formatLabel, statusColor, priorityColor } from './shareView';

	interface Props {
		item: PublicItem;
		fields: FieldDef[];
		/** When true (and onactivate is set), the card advertises itself as an
		 *  expand affordance. Wiring deferred to TASK-1684. */
		expandable?: boolean;
		/** Fired when an expandable card is activated (click / Enter / Space).
		 *  Deferred wiring — TASK-1684. */
		onactivate?: (item: PublicItem) => void;
	}

	let { item, fields, expandable = false, onactivate }: Props = $props();

	let interactive = $derived(expandable && !!onactivate);

	let statusFieldDef = $derived(findField(fields, 'status'));
	let priorityFieldDef = $derived(findField(fields, 'priority'));
	let status = $derived(
		typeof item.fields.status === 'string' ? (item.fields.status as string) : ''
	);
	let priority = $derived(
		typeof item.fields.priority === 'string' ? (item.fields.priority as string) : ''
	);

	function activate() {
		if (interactive) onactivate?.(item);
	}

	function onKey(e: KeyboardEvent) {
		if (!interactive) return;
		if (e.key === 'Enter' || e.key === ' ') {
			e.preventDefault();
			onactivate?.(item);
		}
	}
</script>

<!-- svelte-ignore a11y_no_noninteractive_tabindex -->
<!-- `role` and `tabindex` are correlated at runtime: when tabindex is 0 the
     role is always 'button' (both gated by `interactive`), so the element is
     focusable only when it is genuinely interactive. The analyzer can't see
     that correlation. Wiring lands in TASK-1684. -->
<div
	class="public-card"
	class:interactive
	role={interactive ? 'button' : undefined}
	tabindex={interactive ? 0 : undefined}
	onclick={interactive ? activate : undefined}
	onkeydown={interactive ? onKey : undefined}
>
	{#if item.ref}
		<div class="card-top">
			<span class="card-ref">{item.ref}</span>
		</div>
	{/if}

	<div class="card-title">{item.title}</div>

	{#if (statusFieldDef && status) || (priorityFieldDef && priority)}
		<div class="card-meta">
			{#if statusFieldDef && status}
				<span class="meta-status" style:color={statusColor(status)}>
					{formatLabel(status).toUpperCase()}
				</span>
			{/if}
			{#if priorityFieldDef && priority}
				{#if statusFieldDef && status}<span class="meta-sep">&middot;</span>{/if}
				<span class="meta-priority" style:color={priorityColor(priority)}>
					{formatLabel(priority)}
				</span>
			{/if}
		</div>
	{/if}
</div>

<style>
	.public-card {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		background: var(--bg-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-3) var(--space-4);
		min-width: 0;
	}

	.public-card.interactive {
		cursor: pointer;
		transition: background 0.1s, border-color 0.1s;
	}

	.public-card.interactive:hover {
		background: var(--bg-hover);
		border-color: var(--text-tertiary, var(--text-secondary));
	}

	.public-card.interactive:focus-visible {
		outline: 2px solid var(--accent-blue);
		outline-offset: -2px;
	}

	.card-top {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}

	.card-ref {
		font-family: var(--font-mono);
		font-size: 0.72em;
		color: var(--text-muted);
		font-weight: 400;
		white-space: nowrap;
	}

	.card-title {
		font-size: 0.92em;
		color: var(--text-primary);
		line-height: 1.45;
		font-weight: 600;
		overflow-wrap: anywhere;
		min-width: 0;
	}

	.card-meta {
		display: flex;
		align-items: center;
		gap: 5px;
		flex-wrap: wrap;
		min-width: 0;
	}

	.meta-status {
		font-size: 0.7em;
		font-weight: 700;
		text-transform: uppercase;
		letter-spacing: 0.02em;
		white-space: nowrap;
	}

	.meta-sep {
		font-size: 0.7em;
		color: var(--text-muted);
	}

	.meta-priority {
		font-size: 0.7em;
		font-weight: 600;
		white-space: nowrap;
	}
</style>
