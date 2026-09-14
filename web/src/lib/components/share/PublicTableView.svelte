<script lang="ts">
	// Read-only table renderer for the public share page (TASK-1679).
	//
	// Renders Ref + Title + one column per (non-computed) schema field as a CSS
	// grid (role="table"/"row"/"cell"), matching the in-app TableView's layout
	// and accessibility semantics. Read-only: no sortable headers that mutate,
	// no row links, no status cycling. Status/priority cells get the shared
	// color vocabulary; rows are inert (or expand-only once TASK-1684 wires
	// `onactivate`).
	import type { FieldDef } from '$lib/types';
	import type { PublicCollection, PublicItem } from './shareView';
	import {
		visibleFields,
		formatLabel,
		formatFieldValue,
		fieldValueColor,
		publicRelationText,
		PUBLIC_RELATION_TITLE,
	} from './shareView';
	import { isRelationType } from '$lib/items/relationFieldTypes';
	import PublicItemExpansion from './PublicItemExpansion.svelte';

	interface Props {
		collection: PublicCollection;
		items: PublicItem[];
		/** Inline read-only expand affordance (TASK-1684). */
		expandable?: boolean;
		onactivate?: (item: PublicItem) => void;
		/** `key` of the currently-expanded item ('' = none). */
		expandedKey?: string;
		/** Returns pre-sanitized HTML for an item's markdown body (route-owned). */
		renderContent?: (item: PublicItem) => string;
	}

	let { collection, items, expandable = false, onactivate, expandedKey = '', renderContent }: Props =
		$props();

	let interactive = $derived(expandable && !!onactivate);
	let columns = $derived(visibleFields(collection.fields));
	let hasRefs = $derived(items.some((i) => !!i.ref));

	/* Extrinsic tracks only (no `auto`): rows use grid-template-columns:
	 * inherit, and content-visibility's layout containment disables
	 * subgrid per spec (TASK-2208) — see TableView.svelte for the full
	 * rationale. */
	let gridTemplate = $derived(
		[
			...(hasRefs ? ['70px'] : []),
			'minmax(200px, 1fr)',
			...columns.map(() => 'minmax(90px, 0.55fr)')
		].join(' ')
	);

	function panelIdFor(item: PublicItem): string {
		return `pub-exp-${item.key.replace(/[^a-zA-Z0-9_-]/g, '-')}`;
	}

	// Color status/priority cells (and any other select-type field) using the
	// schema-driven resolver so custom terminal vocabularies match the owner's
	// palette. Non-select fields render plain text (no color).
	function cellColor(field: FieldDef, value: string): string | undefined {
		if (field.key === 'status' || field.key === 'priority' || field.type === 'select') {
			return fieldValueColor(field, value);
		}
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
		{#each items as item (item.key)}
			{@const expanded = interactive && expandedKey === item.key}
			<div
				class="table-row"
				class:interactive
				class:expanded
				role="row"
				tabindex={interactive ? 0 : undefined}
				aria-expanded={interactive ? expanded : undefined}
				aria-controls={expanded ? panelIdFor(item) : undefined}
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
					{@const color = typeof raw === 'string' ? cellColor(field, raw) : undefined}
					<div class="table-cell" role="cell">
						<!--
							A relation NEVER prints its stored value here (BUG-3016): the
							value is an item id and this payload has no index to resolve it
							against, so the cell says what it holds instead. See
							`publicRelationText` for why resolving it is a visibility
							question rather than a payload one, and IDEA-3066 for the unit
							that would answer it.

							FIRST, like the authenticated table's arm: `options` survives a
							retype, so a `status` that is now a relation would otherwise
							take the chip arm below and print the id as a status pill.
						-->
						{#if isRelationType(field.type)}
							{@const relText = publicRelationText(field, raw)}
							{#if relText}
								<span class="cell-value is-placeholder" title={PUBLIC_RELATION_TITLE}>{relText}</span>
							{:else}
								<span class="cell-value"></span>
							{/if}
						{:else if (field.key === 'status' || field.key === 'priority') && color && text}
							<!-- Tinted chip pill (Phase 3 card language); .cell-status kept
							     as the status cell's stable hook. -->
							<span
								class="cell-chip"
								class:cell-status={field.key === 'status'}
								style:--chip-c={color}>{formatLabel(text)}</span
							>
						{:else if field.type === 'multi_select' && Array.isArray(raw) && raw.length > 0}
							<span class="cell-tags">
								{#each raw as tag, i (i)}
									<span class="cell-tag">{formatFieldValue(tag)}</span>
								{/each}
							</span>
						{:else if color && text}
							<span class="cell-value" style:color>{formatLabel(text)}</span>
						{:else}
							<span class="cell-value">{text}{field.suffix && text ? ` ${field.suffix}` : ''}</span>
						{/if}
					</div>
				{/each}
			</div>
			{#if expanded}
				<!-- Expansion spans the full grid width; presentational row, not a
				     data row, so role="presentation" keeps it out of the table
				     semantics. -->
				<div class="table-expansion-row" role="presentation">
					<PublicItemExpansion
						{item}
						fields={collection.fields}
						html={renderContent?.(item) ?? ''}
						id={panelIdFor(item)}
					/>
				</div>
			{/if}
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

	/* inherit, not subgrid: content-visibility containment disables subgrid
	   on the same element (TASK-2208); template is fully extrinsic so every
	   row aligns identically. */
	.table-row {
		display: grid;
		grid-template-columns: inherit;
		grid-column: 1 / -1;
		border-bottom: 1px solid var(--border-subtle, var(--border));
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
		outline: 2px solid var(--accent-primary, var(--accent-blue));
		outline-offset: -2px;
	}

	.table-row.expanded:not(.table-header) {
		background: var(--bg-secondary);
		border-bottom: none;
	}

	/* Full-width expansion row. Spans every grid column and opts out of subgrid
	   so the panel lays out as a normal block, not against the column tracks. */
	.table-expansion-row {
		grid-column: 1 / -1;
		border-bottom: 1px solid var(--border-subtle, var(--border));
		min-width: 0;
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

	/* A value this share cannot resolve (BUG-3016) — muted and italic so it
	   reads as a note about the cell rather than as the cell's content. */
	.cell-value.is-placeholder {
		color: var(--text-muted);
		font-style: italic;
	}

	/* Status/priority as tinted chip pills — the in-app Chip primitive's `sm`
	   metrics; `--chip-c` set inline from the schema-aware cellColor. */
	.cell-chip {
		display: inline-flex;
		align-items: center;
		padding: 1px 6px;
		border-radius: 6px;
		font-size: 0.8em;
		font-weight: 500;
		line-height: 1.5;
		white-space: nowrap;
		background: color-mix(in srgb, var(--chip-c, var(--accent-gray)) var(--chip-alpha, 16%), transparent);
		color: color-mix(in srgb, var(--chip-c, var(--accent-gray)) var(--chip-text-mix, 100%), #000);
	}

	.cell-tags {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-1, 0.25rem);
		min-width: 0;
	}

	/* Multi-select values as purple tag pills — mirrors ItemCard's .card-tag
	   tint treatment. */
	.cell-tag {
		display: inline-flex;
		align-items: center;
		padding: 1px 6px;
		border-radius: 6px;
		font-size: 0.78em;
		font-weight: 500;
		line-height: 1.5;
		background: color-mix(in srgb, var(--accent-purple) var(--chip-alpha, 16%), transparent);
		color: color-mix(in srgb, var(--accent-purple) var(--chip-text-mix, 100%), #000);
		max-width: 12rem;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
</style>
