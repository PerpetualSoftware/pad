<script lang="ts">
	import type { Item, Collection } from '$lib/types';
	import { parseSchema, parseFields, formatItemRef, itemUrlId } from '$lib/types';
	import { itemComparator, type SortMode } from '$lib/collections/itemSort';
	import { reorderGroup, disabledDirections, type ReorderDirection } from '$lib/collections/reorder';
	import { page } from '$app/state';
	import { statusColor, priorityColor, hasCanonicalStatus, formatFieldLabel as formatLabel } from '$lib/utils/fieldColors';
	import Chip from '$lib/components/common/Chip.svelte';
	import EmptyState from '../common/EmptyState.svelte';
	import ItemActionsMenu from './ItemActionsMenu.svelte';

	interface Props {
		items: Item[];
		collection: Collection;
		wsSlug?: string;
		onStatusChange?: (item: Item, newStatus: string) => void | Promise<void>;
		oncreate?: () => void;
		itemProgress?: Record<string, { total: number; done: number }>;
		progressLabel?: string;
		/**
		 * Reorder plumbing (IDEA-1898). Unlike List/Board, the table had no
		 * manual-order surface — it only column-header-sorts ephemerally. To
		 * support the reorder menu it now honors the page-wide `sortMode`
		 * (manual ⇒ stored sort_order) when no column-header sort is active,
		 * and persists moves through `onReorder`.
		 */
		onReorder?: (updates: { slug: string; sort_order: number }[]) => void;
		canEdit?: boolean;
		preserveOrder?: boolean;
		sortMode?: SortMode;
		/**
		 * Opt-in split-pane open (PLAN-2105 / TASK-2111). When set, a plain
		 * left-click on the title link opens the item in the collection page's
		 * detail pane; modifier/middle clicks fall through to the `href`
		 * (full-page popout). Omitted everywhere except the collection page.
		 */
		onItemOpen?: (item: Item) => void;
		/**
		 * Highlights the row whose detail pane is open (PLAN-2105 / TASK-2112),
		 * mirroring the focused-row marker List/Board already show. Null =
		 * nothing highlighted.
		 */
		focusedItemId?: string | null;
	}

	let {
		items,
		collection,
		wsSlug = '',
		onStatusChange,
		oncreate,
		itemProgress,
		progressLabel,
		onReorder,
		canEdit = true,
		preserveOrder = false,
		sortMode = 'manual',
		onItemOpen,
		focusedItemId = null
	}: Props = $props();

	// Split-pane row-click interception (PLAN-2105 / TASK-2111). Mirrors
	// ItemCard: only a plain left-click opens the pane; modifier/middle
	// clicks fall through to the native <a href> (full-page popout / SSR /
	// right-click-copy).
	function handleTitleClick(e: MouseEvent, item: Item) {
		if (!onItemOpen) return;
		if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
		if (e.defaultPrevented) return;
		e.preventDefault();
		onItemOpen(item);
	}

	let resolvedWsSlug = $derived(wsSlug || page.params.workspace || '');
	let resolvedUsername = $derived(page.params.username || '');
	let schema = $derived(parseSchema(collection));
	let visibleFields = $derived(schema.fields.filter((f) => !f.computed));

	let sortKey = $state('');
	let sortDir = $state<'asc' | 'desc'>('asc');

	let sortedItems = $derived.by(() => {
		// No column-header sort active: fall back to the page-wide sort so
		// the table reflects the same order as List/Board (manual ⇒ stored
		// sort_order). `preserveOrder` (search active) keeps the parent's
		// relevance order untouched.
		if (!sortKey) {
			return preserveOrder ? items : [...items].sort(itemComparator(sortMode, collection));
		}

		const sorted = [...items].sort((a, b) => {
			let aVal: any;
			let bVal: any;

			if (sortKey === 'title') {
				aVal = a.title;
				bVal = b.title;
			} else {
				const aFields = parseFields(a);
				const bFields = parseFields(b);
				aVal = aFields[sortKey] ?? '';
				bVal = bFields[sortKey] ?? '';
			}

			if (typeof aVal === 'number' && typeof bVal === 'number') {
				return sortDir === 'asc' ? aVal - bVal : bVal - aVal;
			}

			const aStr = String(aVal).toLowerCase();
			const bStr = String(bVal).toLowerCase();
			const cmp = aStr.localeCompare(bStr);
			return sortDir === 'asc' ? cmp : -cmp;
		});

		return sorted;
	});

	function toggleSort(key: string) {
		if (sortKey === key) {
			sortDir = sortDir === 'asc' ? 'desc' : 'asc';
		} else {
			sortKey = key;
			sortDir = 'asc';
		}
	}

	// Reorder is only meaningful in manual sort with no column-header
	// override (a column sort would immediately re-order the rows) and not
	// while search preserves relevance order. A column-header click thus
	// transparently hides the menu until the user clears it back to the
	// default (manual) order. The table is a flat list, so the group is the
	// whole displayed set.
	let canReorder = $derived(canEdit && sortMode === 'manual' && !sortKey && !preserveOrder && !!onReorder);

	function reorderItem(item: Item, dir: ReorderDirection) {
		if (!onReorder) return;
		const updates = reorderGroup(sortedItems, item.id, dir);
		if (updates.length > 0) {
			onReorder(updates.map((u) => ({ slug: u.item.id, sort_order: u.sort_order })));
		}
	}

	// Per-row pulse for the status click-cycle chip (mirrors ItemCard's
	// `pulsing`, keyed by item id since the table renders many rows).
	let pulsingId = $state<string | null>(null);

	function cycleStatus(item: Item, options: string[]) {
		if (!onStatusChange) return;
		const fields = parseFields(item);
		const current = fields.status ?? '';
		const idx = options.indexOf(current);
		const next = options[(idx + 1) % options.length];
		pulsingId = item.id;
		setTimeout(() => {
			if (pulsingId === item.id) pulsingId = null;
		}, 300);
		onStatusChange(item, next);
	}

	/** FieldEditor's canonical-value resolution: canonical statuses + the four
	 *  priorities get the fieldColors palette; unknown values return null so
	 *  the cell renders as plain text instead of a chip. */
	function selectValueColor(val: string): string | null {
		if (hasCanonicalStatus(val)) return statusColor(val);
		const p = val?.toLowerCase();
		if (p === 'critical' || p === 'high' || p === 'medium' || p === 'low') {
			return priorityColor(val);
		}
		return null;
	}

	function relativeTime(dateStr: string): string {
		const now = Date.now();
		const then = new Date(dateStr).getTime();
		const diff = now - then;
		const minutes = Math.floor(diff / 60000);
		if (minutes < 1) return 'just now';
		if (minutes < 60) return `${minutes}m ago`;
		const hours = Math.floor(minutes / 60);
		if (hours < 24) return `${hours}h ago`;
		const days = Math.floor(hours / 24);
		if (days < 30) return `${days}d ago`;
		return new Date(dateStr).toLocaleDateString();
	}

	/**
	 * Virtualization (TASK-1348 / PLAN-1343 Phase 1).
	 *
	 * Each row gets `content-visibility: auto` so the browser skips
	 * layout/style/paint for off-screen rows. The catch: CSS
	 * Containment L2 §4.4 makes layout/paint containment a no-op on
	 * internal table boxes (table-row, table-cell, etc.), and
	 * content-visibility's skip behavior depends on size containment,
	 * which also no-ops on table rows. So the table-row branch of CSS
	 * Containment defeats the trick that worked for ListView (PR #488)
	 * and BoardView (PR #489).
	 *
	 * Fix: render the table as a CSS Grid (`<div role="table">` /
	 * `<div role="row">`) instead of `<table>`. The grid layout
	 * preserves the table's column alignment, sticky header, hover
	 * states, and visual fidelity. Rows are no longer "internal table
	 * boxes," so content-visibility / containment apply normally. ARIA
	 * roles preserve assistive-tech semantics — Codex round 1 [P2] on
	 * PR #490 traced through the spec citation to this conclusion.
	 *
	 * The grid template is built dynamically because `visibleFields`
	 * depends on the collection schema. Fixed-width Ref / Updated
	 * columns bracket a `minmax(200px, 1fr)` Title and `auto`-sized
	 * field columns, matching the pre-refactor `.col-*` widths.
	 */
	/* Field columns are EXTRINSIC (minmax + fr, no `auto`): every row
	 * resolves the same track list against the same width, so per-row
	 * `grid-template-columns: inherit` aligns identically WITHOUT subgrid.
	 * That matters because content-visibility:auto implies layout
	 * containment, which disables subgrid on the same element per spec —
	 * rows collapsed to a single stacked column in Chromium (TASK-2208). */
	let gridTemplate = $derived(
		[
			'70px',
			'minmax(200px, 1fr)',
			...visibleFields.map(() => 'minmax(90px, 0.55fr)'),
			'90px',
			...(canReorder ? ['44px'] : [])
		].join(' ')
	);
</script>

{#if items.length === 0}
	<EmptyState {collection} wsSlug={resolvedWsSlug} {oncreate} />
{:else}
<div class="table-scroll">
	<div class="table-view" role="table" style:grid-template-columns={gridTemplate}>
		<div class="table-row table-header" role="row">
			<div class="table-cell col-ref" role="columnheader">Ref</div>
			<div class="table-cell col-title" role="columnheader">
				<button class="sort-btn" onclick={() => toggleSort('title')}>
					Title {sortKey === 'title' ? (sortDir === 'asc' ? '↑' : '↓') : ''}
				</button>
			</div>
			{#each visibleFields as field (field.key)}
				<div class="table-cell" role="columnheader">
					<button class="sort-btn" onclick={() => toggleSort(field.key)}>
						{field.label || field.key} {sortKey === field.key ? (sortDir === 'asc' ? '↑' : '↓') : ''}
					</button>
				</div>
			{/each}
			<div class="table-cell col-updated" role="columnheader">Updated</div>
			{#if canReorder}
				<div class="table-cell col-actions" role="columnheader"><span class="sr-only">Reorder</span></div>
			{/if}
		</div>
		{#each sortedItems as item, i (item.id)}
			{@const fields = parseFields(item)}
			<div class="table-row" class:focused={focusedItemId === item.id} role="row">
				<div class="table-cell col-ref" role="cell"><span class="ref">{formatItemRef(item) ?? ''}</span></div>
				<div class="table-cell col-title" role="cell">
					<a href="/{resolvedUsername}/{resolvedWsSlug}/{collection.slug}/{itemUrlId(item)}" class="title-link" onclick={(e) => handleTitleClick(e, item)}>{item.title}</a>
					{#if itemProgress?.[item.id]}
						{@const p = itemProgress[item.id]}
						<div class="cell-progress">
							<div class="cell-progress-bar"><div class="cell-progress-fill" style:width="{Math.round((p.done / p.total) * 100)}%"></div></div>
							<span class="cell-progress-text">{p.done}/{p.total}</span>
						</div>
					{/if}
				</div>
				{#each visibleFields as field (field.key)}
					<div class="table-cell" role="cell">
						{#if field.key === 'status' && field.options && onStatusChange}
							<Chip
								size="sm"
								color={statusColor(fields[field.key] ?? '')}
								pulse={pulsingId === item.id}
								onclick={() => cycleStatus(item, field.options!)}
								title="Click to cycle status"
							>
								{formatLabel(fields[field.key] ?? '')}
							</Chip>
						{:else if field.options && typeof fields[field.key] === 'string' && fields[field.key]}
							{@const chipColor = selectValueColor(fields[field.key])}
							{#if chipColor}
								<Chip size="sm" color={chipColor}>{formatLabel(fields[field.key])}</Chip>
							{:else}
								<span class="cell-value">{fields[field.key]}</span>
							{/if}
						{:else}
							<span class="cell-value">{fields[field.key] ?? ''}</span>
						{/if}
					</div>
				{/each}
				<div class="table-cell col-updated" role="cell"><span class="cell-date">{relativeTime(item.updated_at)}</span></div>
				{#if canReorder}
					<div class="table-cell col-actions" role="cell">
						<ItemActionsMenu
							{item}
							label={item.title}
							disabledDirs={disabledDirections(i, sortedItems.length)}
							onReorder={(dir) => reorderItem(item, dir)}
						/>
					</div>
				{/if}
			</div>
		{/each}
	</div>
</div>
{/if}

<style>
	.table-scroll {
		overflow-x: auto;
		-webkit-overflow-scrolling: touch;
	}

	.table-view {
		display: grid;
		/* grid-template-columns is set inline via style:grid-template-columns
		   because the column count depends on the collection schema. */
		width: 100%;
		font-size: 0.88em;
	}

	/*
	 * Rows inherit the parent's (fully extrinsic) column template instead
	 * of using subgrid: content-visibility:auto implies layout containment,
	 * which DISABLES subgrid on the same element per spec — every row
	 * collapsed to a single stacked column in Chromium (TASK-2208). With
	 * no `auto` tracks in the template, `inherit` yields pixel-identical
	 * columns on every row, and rows stay the unit content-visibility
	 * can skip.
	 */
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

	/*
	 * Virtualization rule. Applies to every body row (the header row is
	 * excluded so sticky positioning isn't fought by paint skipping).
	 *
	 *   - content-visibility: auto — engine skips work when off-screen
	 *   - contain-intrinsic-size: auto 36px — placeholder height for
	 *     unrendered rows; the `auto` keyword caches measured heights
	 *     so rows with progress bars or wrapped titles keep their real
	 *     size on re-entry
	 *
	 * No DnD, no protruding badges, no absolute-positioned overflow on
	 * cells, so no `overflow-clip-margin` escape hatch is required (cf.
	 * PR #489's pr-badge handling on BoardView).
	 */
	.table-row:not(.table-header) {
		content-visibility: auto;
		contain-intrinsic-size: auto 36px;
	}

	.table-row:not(.table-header):hover {
		background: var(--bg-hover);
	}

	/* Highlight the row whose detail pane is open (PLAN-2105 / TASK-2112).
	   Violet tint + left accent bar — the table-row adaptation of ItemCard's
	   selected ring (PLAN-2290 Phase 3). E2E asserts the .focused CLASS, not
	   these styles — keep the class name stable. */
	.table-row:not(.table-header).focused {
		background: color-mix(in srgb, var(--accent-primary, var(--accent-blue)) 7%, transparent);
		box-shadow: inset 2px 0 0 var(--accent-primary, var(--accent-blue));
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

	.sort-btn {
		background: none;
		border: none;
		color: inherit;
		font: inherit;
		font-weight: 600;
		cursor: pointer;
		padding: 0;
		white-space: nowrap;
		text-align: left;
	}

	.sort-btn:hover {
		color: var(--accent-blue);
	}

	/* .col-ref and .col-updated widths come from grid-template-columns;
	   .col-title only needs the column-stack treatment for its progress bar. */
	.col-title { min-width: 0; flex-direction: column; align-items: flex-start; }

	.ref {
		font-family: var(--font-mono);
		font-size: 0.85em;
		color: var(--text-muted);
	}

	.title-link {
		color: var(--text-primary);
		text-decoration: none;
		font-weight: 500;
	}

	.title-link:hover {
		color: var(--accent-blue);
	}

	.cell-value {
		color: var(--text-secondary);
		font-size: 0.9em;
	}

	/* Status + recognized select values render as Chip primitives now
	   (tinted pills per the refresh mock); the chip carries the
	   click-cycle + pulse. */

	.cell-date {
		font-size: 0.8em;
		color: var(--text-muted);
		white-space: nowrap;
	}

	.cell-progress {
		display: flex;
		align-items: center;
		gap: 4px;
		margin-top: 2px;
		width: 100%;
	}

	.cell-progress-bar {
		flex: 1;
		height: 3px;
		background: var(--bg-tertiary);
		border-radius: 2px;
		max-width: 60px;
	}

	.cell-progress-fill {
		height: 100%;
		background: var(--accent-green);
		border-radius: 2px;
	}

	.cell-progress-text {
		font-size: 0.7em;
		color: var(--text-muted);
	}

	.col-actions {
		justify-content: center;
		padding-left: 0;
		padding-right: var(--space-2);
	}

	.sr-only {
		position: absolute;
		width: 1px;
		height: 1px;
		padding: 0;
		margin: -1px;
		overflow: hidden;
		clip: rect(0, 0, 0, 0);
		white-space: nowrap;
		border: 0;
	}
</style>
