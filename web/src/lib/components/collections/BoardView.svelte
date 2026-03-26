<script lang="ts">
	import type { Item, Collection } from '$lib/types';
	import { parseSchema, parseFields } from '$lib/types';
	import { dndzone, TRIGGERS, SHADOW_ITEM_MARKER_PROPERTY_NAME } from 'svelte-dnd-action';
	import type { DndEvent } from 'svelte-dnd-action';
	import ItemCard from './ItemCard.svelte';


	interface Props {
		items: Item[];
		collection: Collection;
		groupField?: string;
		onStatusChange: (item: Item, newStatus: string) => void;
		onReorder?: (updates: { slug: string; sort_order: number }[]) => void;
		onArchiveColumn?: (items: Item[]) => void;
		itemProgress?: Record<string, { total: number; done: number }>;
		relationLabels?: Record<string, string>;
	}

	let { items, collection, groupField = 'status', onStatusChange, onReorder, onArchiveColumn, itemProgress, relationLabels }: Props = $props();

	let confirmArchiveColumn = $state<string | null>(null);

	const flipDurationMs = 200;
	const touchDragDelayMs = 500;

	let schema = $derived(parseSchema(collection));
	let field = $derived(schema.fields.find((f) => f.key === groupField));
	let columns = $derived(field?.options ?? []);

	let isDragging = $state(false);
	let columnData: Record<string, Item[]> = $state({});

	/**
	 * Derived column data from props, grouped by the groupField value
	 * and sorted by sort_order within each column.
	 * This is the "source of truth" from props and always reflects the latest items.
	 */
	let propColumnData = $derived.by(() => {
		const result: Record<string, Item[]> = {};
		for (const col of columns) {
			result[col] = [];
		}
		for (const item of items) {
			const fields = parseFields(item);
			const value = fields[groupField] ?? '';
			if (result[value]) {
				result[value].push(item);
			}
		}
		for (const col of columns) {
			result[col].sort((a, b) => a.sort_order - b.sort_order);
		}
		return result;
	});

	/**
	 * Sync the mutable columnData from the derived prop data,
	 * but only when the user is not actively dragging.
	 * During a drag, svelte-dnd-action mutates columnData directly
	 * via handleConsider/handleFinalize.
	 */
	$effect(() => {
		const data = propColumnData;
		if (!isDragging) {
			columnData = data;
		}
	});

	function handleConsider(columnValue: string, e: CustomEvent<DndEvent<Item>>) {
		columnData[columnValue] = e.detail.items;
		if (!isDragging && e.detail.info.trigger === TRIGGERS.DRAG_STARTED) {
			if (typeof navigator !== 'undefined' && navigator.vibrate) {
				navigator.vibrate(50);
			}
		}
		isDragging = true;
	}

	function handleFinalize(columnValue: string, e: CustomEvent<DndEvent<Item>>) {
		columnData[columnValue] = e.detail.items;
		isDragging = false;

		const { id: itemId, trigger } = e.detail.info;

		if (trigger === TRIGGERS.DROPPED_INTO_ZONE) {
			const originalItem = items.find((i) => i.id === itemId);
			if (originalItem) {
				const fields = parseFields(originalItem);
				if (fields[groupField] !== columnValue) {
					onStatusChange(originalItem, columnValue);
				}
			}
		}

		if (onReorder) {
			const updates = columnData[columnValue]
				.filter((i: any) => !i[SHADOW_ITEM_MARKER_PROPERTY_NAME])
				.map((item, index) => ({ slug: item.slug, sort_order: index }));
			if (updates.length > 0) {
				onReorder(updates);
			}
		}
	}

	function formatLabel(value: string): string {
		return value.replace(/_/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase());
	}

	function columnCssClass(value: string): string {
		switch (value) {
			case 'in_progress':
				return 'col-in-progress';
			case 'done':
				return 'col-done';
			case 'blocked':
				return 'col-blocked';
			default:
				return '';
		}
	}
</script>

<div class="board-view" style:--col-count={columns.length}>
	{#each columns as colValue (colValue)}
		{@const colItems = columnData[colValue] ?? []}
		<div class="kanban-column" role="group" aria-label="{formatLabel(colValue)} column">
			<div class="column-header {columnCssClass(colValue)}">
				<span class="column-name">{formatLabel(colValue)}</span>
				<div class="column-actions">
					<span class="column-count">{colItems.length}</span>
					{#if onArchiveColumn && colItems.length > 0}
						{#if confirmArchiveColumn === colValue}
							<span class="archive-confirm">
								<button class="archive-yes" onclick={() => { onArchiveColumn(colItems); confirmArchiveColumn = null; }}>Archive {colItems.length}?</button>
								<button class="archive-no" onclick={() => confirmArchiveColumn = null}>Cancel</button>
							</span>
						{:else}
							<button
								class="archive-col-btn"
								title="Archive all {formatLabel(colValue).toLowerCase()} items"
								onclick={() => confirmArchiveColumn = colValue}
							>&#128451;</button>
						{/if}
					{/if}
				</div>
			</div>
			<!-- svelte-ignore a11y_no_static_element_interactions -->
			<div
				class="column-cards"
				use:dndzone={{
					items: colItems,
					flipDurationMs,
					type: 'board-card',
					dropTargetClasses: ['drop-target'],
					delayTouchStart: touchDragDelayMs
				}}
				onconsider={(e) => handleConsider(colValue, e)}
				onfinalize={(e) => handleFinalize(colValue, e)}
				oncontextmenu={(e) => e.preventDefault()}
			>
				{#each colItems as item (item.id)}
					<div class="card-wrapper">
						<ItemCard
							{item}
							{collection}
							compact={true}
							statusOptions={columns}
							onStatusClick={onStatusChange}
							progress={itemProgress?.[item.id] ?? null}
							{relationLabels}
						/>
					</div>
				{/each}
			</div>
			{#if colItems.length === 0 && !isDragging}
				<div class="column-empty">No {formatLabel(colValue).toLowerCase()} items</div>
			{/if}
		</div>
	{/each}
</div>

<style>
	.board-view {
		display: grid;
		grid-template-columns: repeat(var(--col-count, 3), 1fr);
		gap: var(--space-5);
	}

	.kanban-column {
		display: flex;
		flex-direction: column;
		min-width: 0;
	}

	.column-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-2) var(--space-3);
		margin-bottom: var(--space-3);
		border-bottom: 2px solid var(--text-secondary);
		font-weight: 600;
		font-size: 0.9em;
	}

	.column-actions {
		display: flex;
		align-items: center;
		gap: var(--space-1);
	}

	.column-header.col-in-progress {
		border-bottom-color: var(--accent-amber);
	}

	.column-header.col-done {
		border-bottom-color: var(--accent-green);
	}

	.column-header.col-blocked {
		border-bottom-color: var(--accent-orange);
	}

	.column-name {
		color: var(--text-primary);
	}

	.column-count {
		font-size: 0.8em;
		font-weight: 400;
		color: var(--text-muted);
		background: var(--bg-tertiary);
		padding: 1px 8px;
		border-radius: 10px;
	}

	.archive-col-btn {
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 0.8em;
		cursor: pointer;
		padding: 2px 4px;
		border-radius: var(--radius-sm);
		opacity: 0;
		transition: opacity 0.15s;
		line-height: 1;
	}

	.column-header:hover .archive-col-btn {
		opacity: 1;
	}

	.archive-col-btn:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
	}

	.archive-confirm {
		display: flex;
		gap: var(--space-1);
		align-items: center;
	}

	.archive-yes {
		background: none;
		border: none;
		color: var(--accent-red, #ef4444);
		font-size: 0.75em;
		cursor: pointer;
		padding: 2px 6px;
		border-radius: var(--radius-sm);
		white-space: nowrap;
	}

	.archive-yes:hover {
		background: color-mix(in srgb, var(--accent-red, #ef4444) 10%, transparent);
	}

	.archive-no {
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 0.75em;
		cursor: pointer;
		padding: 2px 6px;
		border-radius: var(--radius-sm);
	}

	.archive-no:hover {
		color: var(--text-primary);
	}

	.column-cards {
		display: flex;
		flex-direction: column;
		flex: 1;
		gap: var(--space-3);
		min-height: 60px;
		border-radius: var(--radius);
		padding: var(--space-2);
		transition: background 0.15s ease;
	}

	.column-cards:global(.drop-target) {
		background: color-mix(in srgb, var(--accent-blue) 6%, transparent);
	}

	.card-wrapper {
		cursor: grab;
		-webkit-touch-callout: none;
		-webkit-user-select: none;
		user-select: none;
	}

	.card-wrapper:active {
		cursor: grabbing;
	}

	.column-empty {
		text-align: center;
		padding: var(--space-4);
		color: var(--text-muted);
		font-size: 0.82em;
	}

	@media (max-width: 768px) {
		.board-view {
			grid-template-columns: 1fr;
		}
	}
</style>
