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
		onStatusChange: (item: Item, newStatus: string) => void | Promise<void>;
		onReorder?: (updates: { slug: string; sort_order: number }[]) => void;
		onArchiveColumn?: (items: Item[]) => void;
		onGroupReorder?: (newOrder: string[]) => void;
		itemProgress?: Record<string, { total: number; done: number }>;
		progressLabel?: string;
		relationLabels?: Record<string, string>;
	}

	let { items, collection, groupField = 'status', onStatusChange, onReorder, onArchiveColumn, onGroupReorder, itemProgress, progressLabel = 'tasks', relationLabels }: Props = $props();

	let confirmArchiveColumn = $state<string | null>(null);

	const flipDurationMs = 200;
	const touchDragDelayMs = 500;

	let schema = $derived(parseSchema(collection));
	let field = $derived(schema.fields.find((f) => f.key === groupField));
	let columns = $derived(field?.options ?? []);

	// Column order state — tracks the displayed order, syncs from schema when not dragging
	let columnOrder = $state<string[]>([]);

	$effect(() => {
		columnOrder = [...columns];
	});

	// Native HTML5 drag-and-drop for column reordering
	let draggedColumn = $state<string | null>(null);
	let dragOverColumn = $state<string | null>(null);

	function handleColumnDragStart(e: DragEvent, colValue: string) {
		draggedColumn = colValue;
		if (e.dataTransfer) {
			e.dataTransfer.effectAllowed = 'move';
			e.dataTransfer.setData('text/plain', colValue);
		}
	}

	function handleColumnDragOver(e: DragEvent, colValue: string) {
		if (!draggedColumn || draggedColumn === colValue) return;
		e.preventDefault();
		if (e.dataTransfer) e.dataTransfer.dropEffect = 'move';
		dragOverColumn = colValue;
	}

	function handleColumnDragLeave() {
		dragOverColumn = null;
	}

	function handleColumnDrop(e: DragEvent, colValue: string) {
		e.preventDefault();
		if (!draggedColumn || draggedColumn === colValue) return;

		const fromIdx = columnOrder.indexOf(draggedColumn);
		const toIdx = columnOrder.indexOf(colValue);
		if (fromIdx === -1 || toIdx === -1) return;

		const newOrder = [...columnOrder];
		newOrder.splice(fromIdx, 1);
		newOrder.splice(toIdx, 0, draggedColumn);
		columnOrder = newOrder;

		if (onGroupReorder) {
			onGroupReorder(newOrder);
		}

		draggedColumn = null;
		dragOverColumn = null;
	}

	function handleColumnDragEnd() {
		draggedColumn = null;
		dragOverColumn = null;
	}

	let isDragging = $state(false);
	let columnData: Record<string, Item[]> = $state({});

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

	async function handleFinalize(columnValue: string, e: CustomEvent<DndEvent<Item>>) {
		columnData[columnValue] = e.detail.items;

		// Capture the desired order BEFORE setting isDragging = false or awaiting,
		// because both can trigger reactive effects that overwrite columnData.
		const reorderUpdates = columnData[columnValue]
			.filter((i: any) => !i[SHADOW_ITEM_MARKER_PROPERTY_NAME])
			.map((item, index) => ({ slug: item.id, sort_order: index }));

		isDragging = false;

		const { id: itemId, trigger } = e.detail.info;

		if (trigger === TRIGGERS.DROPPED_INTO_ZONE) {
			const originalItem = items.find((i) => i.id === itemId);
			if (originalItem) {
				const fields = parseFields(originalItem);
				if (fields[groupField] !== columnValue) {
					await onStatusChange(originalItem, columnValue);
				}
			}
		}

		if (onReorder && reorderUpdates.length > 0) {
			onReorder(reorderUpdates);
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

<div class="board-view" style:--col-count={columnOrder.length}>
	{#each columnOrder as colValue (colValue)}
		{@const colItems = columnData[colValue] ?? []}
		<div
			class="kanban-column"
			class:drag-over-left={dragOverColumn === colValue}
			class:dragging-source={draggedColumn === colValue}
			role="group"
			aria-label="{formatLabel(colValue)} column"
			ondragover={(e) => handleColumnDragOver(e, colValue)}
			ondragleave={handleColumnDragLeave}
			ondrop={(e) => handleColumnDrop(e, colValue)}
		>
			<div
				class="column-header {columnCssClass(colValue)}"
				draggable="true"
				ondragstart={(e) => handleColumnDragStart(e, colValue)}
				ondragend={handleColumnDragEnd}
			>
				<span class="column-drag-handle" title="Drag to reorder">⠿</span>
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
							{progressLabel}
							{relationLabels}
						/>
					</div>
				{/each}
				{#if colItems.length === 0 && !isDragging}
					<div class="column-empty">No {formatLabel(colValue).toLowerCase()} items</div>
				{/if}
			</div>
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
		transition: transform 0.15s ease;
	}

	.kanban-column.dragging-source {
		opacity: 0.4;
	}

	.kanban-column.drag-over-left {
		box-shadow: -3px 0 0 0 var(--accent-blue);
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
		cursor: grab;
	}

	.column-header:active {
		cursor: grabbing;
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

	.column-drag-handle {
		color: var(--text-muted);
		font-size: 0.75em;
		cursor: grab;
		opacity: 0;
		transition: opacity 0.15s;
		user-select: none;
		margin-right: var(--space-1);
	}

	.column-header:hover .column-drag-handle {
		opacity: 0.5;
	}

	.column-drag-handle:active {
		opacity: 1;
		cursor: grabbing;
	}

	.column-name {
		color: var(--text-primary);
		flex: 1;
		text-align: left;
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
