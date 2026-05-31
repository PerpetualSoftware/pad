<script lang="ts">
	// Read-only kanban renderer for the public share page (TASK-1679).
	//
	// Groups items into columns by the collection's `board_group_by` (or
	// `status`) and renders each column as a static stack of PublicItemCards.
	// No drag/drop, no column reorder, no add/draft, no status mutation — just
	// the owner's columns, presented for an anonymous audience. Mirrors the
	// in-app BoardView's column layout + header accents without its coupling.
	import type { PublicCollection, PublicItem } from './shareView';
	import {
		findField,
		resolveGroupField,
		groupItems,
		formatLabel,
		columnAccentClass
	} from './shareView';
	import PublicItemCard from './PublicItemCard.svelte';

	interface Props {
		collection: PublicCollection;
		items: PublicItem[];
		/** Forwarded to each card for the deferred inline-expand (TASK-1684). */
		expandable?: boolean;
		onactivate?: (item: PublicItem) => void;
	}

	let { collection, items, expandable = false, onactivate }: Props = $props();

	let groupField = $derived(resolveGroupField(collection));
	let optionOrder = $derived(findField(collection.fields, groupField)?.options ?? []);
	let columns = $derived(groupItems(items, groupField, optionOrder));
</script>

<div class="public-board">
	{#each columns as column (column.value)}
		<section class="board-column" aria-label="{formatLabel(column.value) || 'Ungrouped'} column">
			<header class="column-header {columnAccentClass(column.value)}">
				<span class="column-name">{formatLabel(column.value) || 'Ungrouped'}</span>
				<span class="column-count">{column.items.length}</span>
			</header>
			<div class="column-cards">
				{#each column.items as item (item.key)}
					<PublicItemCard {item} fields={collection.fields} {expandable} {onactivate} />
				{/each}
				{#if column.items.length === 0}
					<p class="column-empty">No {(formatLabel(column.value) || 'ungrouped').toLowerCase()} items</p>
				{/if}
			</div>
		</section>
	{/each}
</div>

<style>
	.public-board {
		display: flex;
		gap: var(--space-4);
		align-items: flex-start;
		overflow-x: auto;
		padding-bottom: var(--space-2);
	}

	.board-column {
		display: flex;
		flex-direction: column;
		flex: 1 0 0;
		min-width: 240px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
	}

	.column-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-3) var(--space-4);
		border-bottom: 2px solid var(--text-secondary);
		border-radius: var(--radius-lg) var(--radius-lg) 0 0;
		font-weight: 700;
		font-size: 0.9em;
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

	.column-cards {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		padding: var(--space-2);
	}

	.column-empty {
		text-align: center;
		padding: var(--space-4);
		color: var(--text-muted);
		font-size: 0.82em;
		margin: 0;
	}

	@media (max-width: 768px) {
		.public-board {
			scroll-snap-type: x proximity;
			-webkit-overflow-scrolling: touch;
			gap: var(--space-3);
		}

		.board-column {
			min-width: 80vw;
			max-width: 80vw;
			scroll-snap-align: center;
			flex-shrink: 0;
		}
	}
</style>
