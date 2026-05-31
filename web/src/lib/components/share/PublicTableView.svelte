<script lang="ts">
	// Read-only table renderer for the public share page (TASK-1679).
	//
	// Renders Ref + Title + one column per (non-computed) schema field as a CSS
	// grid (role="table"/"row"/"cell"), matching the in-app TableView's layout
	// and accessibility semantics. Read-only: no sortable headers that mutate,
	// no row links, no status cycling. Status/priority cells get the shared
	// color vocabulary; rows are inert (or expand-only once TASK-1684 wires
	// `onactivate`).
	import type { PublicCollection, PublicItem } from './shareView';
	import {
		visibleFields,
		formatLabel,
		formatFieldValue,
		statusColor,
		priorityColor
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
	let columns = $derived(visibleFields(collection.fields));
	let hasRefs = $derived(items.some((i) => !!i.ref));

	let gridTemplate = $derived(
		[
			...(hasRefs ? ['70px'] : []),
			'minmax(200px, 1fr)',
			...columns.map(() => 'auto')
		].join(' ')
	);

	function cellColor(key: string, value: string): string | undefined {
		if (key === 'status') return statusColor(value);
		if (key === 'priority') return priorityColor(value);
		return undefined;
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

<div class="table-scroll">
	<div class="public-table" role="table" style:grid-template-columns={gridTemplate}>
		<div class="table-row table-header" role="row">
			{#if hasRefs}<div class="table-cell col-ref" role="columnheader">Ref</div>{/if}
			<div class="table-cell" role="columnheader">Title</div>
			{#each columns as field (field.key)}
				<div class="table-cell" role="columnheader">{field.label || formatLabel(field.key)}</div>
			{/each}
		</div>
		{#each items as item (item.ref || item.title)}
			<div
				class="table-row"
				class:interactive
				role="row"
				tabindex={interactive ? 0 : undefined}
				onclick={interactive ? () => activate(item) : undefined}
				onkeydown={interactive ? (e) => onKey(e, item) : undefined}
			>
				{#if hasRefs}
					<div class="table-cell col-ref" role="cell"><span class="ref">{item.ref}</span></div>
				{/if}
				<div class="table-cell col-title" role="cell">
					<span class="title">{item.title}</span>
				</div>
				{#each columns as field (field.key)}
					{@const raw = item.fields[field.key]}
					{@const text = formatFieldValue(raw)}
					{@const color = typeof raw === 'string' ? cellColor(field.key, raw) : undefined}
					<div class="table-cell" role="cell">
						{#if field.key === 'status' && text}
							<span class="cell-status" style:color>{formatLabel(text).toUpperCase()}</span>
						{:else if color && text}
							<span class="cell-value" style:color>{formatLabel(text)}</span>
						{:else}
							<span class="cell-value">{text}{field.suffix && text ? ` ${field.suffix}` : ''}</span>
						{/if}
					</div>
				{/each}
			</div>
		{/each}
	</div>
</div>

<style>
	.table-scroll {
		overflow-x: auto;
		-webkit-overflow-scrolling: touch;
	}

	.public-table {
		display: grid;
		width: 100%;
		font-size: 0.88em;
	}

	.table-row {
		display: grid;
		grid-template-columns: subgrid;
		grid-column: 1 / -1;
		border-bottom: 1px solid var(--border-subtle, var(--border));
	}

	@supports not (grid-template-columns: subgrid) {
		.table-row {
			grid-template-columns: inherit;
		}
	}

	.table-row.table-header {
		position: sticky;
		top: 0;
		background: var(--bg-primary);
		z-index: 1;
		border-bottom: 2px solid var(--border);
	}

	.table-row:not(.table-header) {
		content-visibility: auto;
		contain-intrinsic-size: auto 36px;
	}

	.table-row.interactive:not(.table-header) {
		cursor: pointer;
	}
	.table-row.interactive:not(.table-header):hover {
		background: var(--bg-hover);
	}
	.table-row.interactive:focus-visible {
		outline: 2px solid var(--accent-blue);
		outline-offset: -2px;
	}

	.table-cell {
		padding: var(--space-2) var(--space-3);
		display: flex;
		align-items: center;
		gap: var(--space-2);
		min-width: 0;
	}

	.table-header .table-cell {
		font-weight: 600;
		font-size: 0.85em;
		color: var(--text-secondary);
		white-space: nowrap;
	}

	.col-title {
		min-width: 0;
	}

	.ref {
		font-family: var(--font-mono);
		font-size: 0.85em;
		color: var(--text-muted);
	}

	.title {
		color: var(--text-primary);
		font-weight: 500;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.cell-value {
		color: var(--text-secondary);
		font-size: 0.9em;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.cell-status {
		font-size: 0.78em;
		font-weight: 700;
		text-transform: uppercase;
		letter-spacing: 0.02em;
		white-space: nowrap;
	}
</style>
