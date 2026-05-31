<script lang="ts">
	// Read-only list renderer for the public share page (TASK-1679).
	//
	// Renders items as a vertical stack of rows. When the collection's
	// `list_group_by` is set (and resolves to a real field), rows are split
	// into labeled groups in option order — mirroring the in-app ListView's
	// grouping — otherwise it's a single flat list. No drag, no collapse
	// toggles, no mutation; rows are inert (or expand-only once TASK-1684 wires
	// `onactivate`).
	import type { FieldDef } from '$lib/types';
	import type { PublicCollection, PublicItem } from './shareView';
	import {
		findField,
		groupItems,
		formatLabel,
		fieldValueColor
	} from './shareView';

	interface Props {
		collection: PublicCollection;
		items: PublicItem[];
		/** Deferred inline-expand affordance (TASK-1684). */
		expandable?: boolean;
		onactivate?: (item: PublicItem) => void;
	}

	let { collection, items, expandable = false, onactivate }: Props = $props();

	let interactive = $derived(expandable && !!onactivate);

	let groupField = $derived.by(() => {
		const key = collection.settings.list_group_by;
		return key && findField(collection.fields, key) ? key : '';
	});

	let statusFieldDef = $derived<FieldDef | undefined>(findField(collection.fields, 'status'));
	let priorityFieldDef = $derived<FieldDef | undefined>(findField(collection.fields, 'priority'));

	let groups = $derived.by(() => {
		if (!groupField) return [{ value: '', items }];
		const optionOrder = findField(collection.fields, groupField)?.options ?? [];
		return groupItems(items, groupField, optionOrder);
	});

	function statusOf(item: PublicItem): string {
		return typeof item.fields.status === 'string' ? (item.fields.status as string) : '';
	}
	function priorityOf(item: PublicItem): string {
		return typeof item.fields.priority === 'string' ? (item.fields.priority as string) : '';
	}

	function activate(item: PublicItem) {
		if (interactive) onactivate?.(item);
	}
	function onKey(e: KeyboardEvent, item: PublicItem) {
		if (!interactive) return;
		if (e.key === 'Enter' || e.key === ' ') {
			e.preventDefault();
			onactivate?.(item);
		}
	}
</script>

<div class="public-list">
	{#each groups as group (group.value)}
		{#if groupField}
			<h3 class="group-heading">
				<span>{formatLabel(group.value) || 'Ungrouped'}</span>
				<span class="group-count">{group.items.length}</span>
			</h3>
		{/if}
		<div class="group-rows">
			{#each group.items as item (item.key)}
				{@const status = statusOf(item)}
				{@const priority = priorityOf(item)}
				<!-- svelte-ignore a11y_no_noninteractive_tabindex -->
				<!-- role + tabindex are runtime-correlated (both gated by
				     `interactive`): focusable only when genuinely a button.
				     Wiring lands in TASK-1684. -->
				<div
					class="list-row"
					class:interactive
					role={interactive ? 'button' : undefined}
					tabindex={interactive ? 0 : undefined}
					onclick={interactive ? () => activate(item) : undefined}
					onkeydown={interactive ? (e) => onKey(e, item) : undefined}
				>
					{#if item.ref}<span class="row-ref">{item.ref}</span>{/if}
					<span class="row-title">{item.title}</span>
					<span class="row-meta">
						{#if priorityFieldDef && priority}
							<span class="row-priority" style:color={fieldValueColor(priorityFieldDef, priority)}
								>{formatLabel(priority)}</span
							>
						{/if}
						{#if statusFieldDef && status}
							<span class="row-status" style:color={fieldValueColor(statusFieldDef, status)}
								>{formatLabel(status).toUpperCase()}</span
							>
						{/if}
					</span>
				</div>
			{/each}
			{#if group.items.length === 0}
				<p class="group-empty">No items</p>
			{/if}
		</div>
	{/each}
</div>

<style>
	.public-list {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}

	.group-heading {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		font-size: 0.85em;
		font-weight: 700;
		text-transform: uppercase;
		letter-spacing: 0.03em;
		color: var(--text-secondary);
		margin: 0 0 var(--space-2);
	}

	.group-count {
		font-size: 0.9em;
		font-weight: 400;
		color: var(--text-muted);
		background: var(--bg-tertiary);
		padding: 1px 8px;
		border-radius: 10px;
	}

	.group-rows {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}

	.list-row {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-3) var(--space-4);
		background: var(--bg-secondary);
		border: 1px solid var(--border-subtle, var(--border));
		border-radius: var(--radius);
		min-width: 0;
	}

	.list-row.interactive {
		cursor: pointer;
		transition: background 0.1s;
	}
	.list-row.interactive:hover {
		background: var(--bg-hover);
	}
	.list-row.interactive:focus-visible {
		outline: 2px solid var(--accent-blue);
		outline-offset: -2px;
	}

	.row-ref {
		font-family: var(--font-mono);
		font-size: 0.78em;
		color: var(--text-muted);
		flex-shrink: 0;
	}

	.row-title {
		flex: 1;
		font-weight: 500;
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.row-meta {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		flex-shrink: 0;
	}

	.row-priority {
		font-size: 0.75em;
		font-weight: 600;
		white-space: nowrap;
	}

	.row-status {
		font-size: 0.72em;
		font-weight: 700;
		text-transform: uppercase;
		letter-spacing: 0.02em;
		white-space: nowrap;
	}

	.group-empty {
		color: var(--text-muted);
		font-size: 0.85em;
		padding: var(--space-2) var(--space-4);
		margin: 0;
	}
</style>
