<script lang="ts">
	import type { Item, Collection } from '$lib/types';
	import { parseSchema, parseFields } from '$lib/types';
	import { itemComparator, type SortMode } from '$lib/collections/itemSort';
	import { dndzone, TRIGGERS, SHADOW_ITEM_MARKER_PROPERTY_NAME } from 'svelte-dnd-action';
	import type { DndEvent } from 'svelte-dnd-action';
	import ItemCard from './ItemCard.svelte';
	import EmptyState from '../common/EmptyState.svelte';


	interface Props {
		items: Item[];
		collection: Collection;
		wsSlug?: string;
		groupField?: string;
		focusedItemId?: string | null;
		onStatusChange: (item: Item, newStatus: string) => void | Promise<void>;
		onReorder?: (updates: { slug: string; sort_order: number }[]) => void;
		onArchiveColumn?: (items: Item[]) => void;
		onGroupReorder?: (newOrder: string[]) => void;
		oncreate?: () => void;
		/**
		 * Create an item directly in this lane, pre-filling the lane's
		 * group field with its column value (folds IDEA-1159). The `+`
		 * lane-header button calls this. Gated behind `canEdit`.
		 */
		onCreateInColumn?: (groupValue: string) => void;
		itemProgress?: Record<string, { total: number; done: number }>;
		progressLabel?: string;
		/**
		 * canEdit gates drag-to-reorder, drag-to-status-change, column
		 * reordering, and the archive-column button. See ListView.svelte
		 * for the rationale (zone-level gate; per-item is a follow-up).
		 * Default true preserves behavior in callers that don't pass it.
		 */
		canEdit?: boolean;
		/**
		 * When true, the in-column `sort_order` sort is skipped and the
		 * parent's item order is preserved. Used by the collection page
		 * to surface localSearch's relevance ranking (TASK-1367) —
		 * otherwise the per-column sort would clobber rank order when
		 * two matches share a column.
		 */
		preserveOrder?: boolean;
		/**
		 * Page-wide sort applied within each lane (TASK-1670). 'manual'
		 * (default) keeps the stored sort_order — the drag order. Any
		 * other mode also disables item drag, since reordering a sorted
		 * lane would be meaningless (the comparator would re-sort it).
		 */
		sortMode?: SortMode;
	}

	let { items, collection, wsSlug = '', groupField = 'status', focusedItemId = null, onStatusChange, onReorder, onArchiveColumn, onGroupReorder, oncreate, onCreateInColumn, itemProgress, progressLabel = 'tasks', canEdit = true, preserveOrder = false, sortMode = 'manual' }: Props = $props();

	let confirmArchiveColumn = $state<string | null>(null);
	// Which lane's ⋯ menu is open (null = none). The menu is the new home
	// for the column actions (archive today; move/tag/priority/assign land
	// in TASK-1672). One menu open at a time.
	let openMenuColumn = $state<string | null>(null);
	let isMobile = $state(false);

	function toggleMenu(colValue: string) {
		openMenuColumn = openMenuColumn === colValue ? null : colValue;
		confirmArchiveColumn = null;
	}

	function closeMenu() {
		openMenuColumn = null;
		confirmArchiveColumn = null;
	}

	// Dismiss the open lane menu on any click outside it (mirrors the
	// QuickActionsMenu pattern). The menu markup lives under
	// `.lane-menu-wrap`, so clicks there don't close it.
	function handleWindowClick(e: MouseEvent) {
		if (openMenuColumn === null) return;
		const target = e.target as HTMLElement | null;
		if (!target) return;
		if (!target.closest('.lane-menu-wrap')) closeMenu();
	}

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

	$effect(() => {
		const mql = window.matchMedia('(max-width: 768px)');
		isMobile = mql.matches;
		function onChange(e: MediaQueryListEvent) {
			isMobile = e.matches;
		}
		mql.addEventListener('change', onChange);
		return () => {
			mql.removeEventListener('change', onChange);
		};
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
		// `preserveOrder` opts out of the in-column sort so search rank
		// from the parent isn't overridden — TASK-1367. Otherwise sort
		// each lane by the page-wide sort mode (TASK-1670); 'manual'
		// resolves to the stored sort_order, preserving prior behavior.
		if (!preserveOrder) {
			const cmp = itemComparator(sortMode, collection);
			for (const col of columns) {
				result[col].sort(cmp);
			}
		}
		return result;
	});

	// Cooldown after a drop — suppress syncs while reorder API calls + SSE events settle
	let dropCooldown = $state(false);

	$effect(() => {
		const data = propColumnData;
		if (!isDragging && !dropCooldown) {
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

		const reorderUpdates = columnData[columnValue]
			.filter((i: any) => !i[SHADOW_ITEM_MARKER_PROPERTY_NAME])
			.map((item, index) => ({ slug: item.id, sort_order: index }));

		const { id: itemId, trigger } = e.detail.info;

		isDragging = false;
		dropCooldown = true;

		let moveSucceeded = true;

		if (trigger === TRIGGERS.DROPPED_INTO_ZONE) {
			const originalItem = items.find((i) => i.id === itemId);
			if (originalItem) {
				const fields = parseFields(originalItem);
				if (fields[groupField] !== columnValue) {
					try {
						await onStatusChange(originalItem, columnValue);
					} catch {
						moveSucceeded = false;
					}
				}
			}
		}

		if (moveSucceeded) {
			// Only persist reorder after a successful move
			if (onReorder && reorderUpdates.length > 0) {
				onReorder(reorderUpdates);
			}
			// Let SSE events settle before re-syncing from props
			setTimeout(() => { dropCooldown = false; }, 2000);
		} else {
			// Move failed — immediately restore original positions
			dropCooldown = false;
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

<svelte:window onclick={handleWindowClick} />

{#if items.length === 0}
	<EmptyState {collection} {wsSlug} {oncreate} />
{:else}
<div class="board-view">
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
			<!-- svelte-ignore a11y_no_static_element_interactions -->
			<div
				class="column-header {columnCssClass(colValue)}"
				draggable={canEdit}
				role="toolbar"
				tabindex="0"
				ondragstart={canEdit ? (e) => handleColumnDragStart(e, colValue) : undefined}
				ondragend={canEdit ? handleColumnDragEnd : undefined}
			>
				{#if canEdit}
					<span class="column-drag-handle" title="Drag to reorder">⠿</span>
				{/if}
				<span class="column-name">{formatLabel(colValue)}</span>
				<div class="column-actions">
					<span class="column-count">{colItems.length}</span>
					{#if canEdit}
						{#if onCreateInColumn}
							<button
								class="lane-btn lane-add-btn"
								title="Add item to {formatLabel(colValue).toLowerCase()}"
								aria-label="Add item to {formatLabel(colValue)}"
								onclick={() => onCreateInColumn?.(colValue)}
							>+</button>
						{/if}
						<div class="lane-menu-wrap">
							<button
								class="lane-btn lane-menu-btn"
								title="Lane actions"
								aria-label="{formatLabel(colValue)} lane actions"
								aria-haspopup="menu"
								aria-expanded={openMenuColumn === colValue}
								onclick={(e) => { e.stopPropagation(); toggleMenu(colValue); }}
							>⋯</button>
							{#if openMenuColumn === colValue}
								<!-- stopPropagation on every in-menu click: an
								     onclick that mutates state (e.g. showing the
								     archive confirm) re-renders and detaches the
								     clicked button before the event bubbles to
								     <svelte:window onclick>, where closest() on the
								     now-orphaned node returns null and slams the
								     menu shut. Same Svelte 5 same-click bubbling
								     issue documented in console/+layout.svelte. -->
								<div class="lane-menu" role="menu">
									{#if onCreateInColumn}
										<button
											class="lane-menu-item"
											role="menuitem"
											onclick={(e) => { e.stopPropagation(); onCreateInColumn?.(colValue); closeMenu(); }}
										>
											<span class="lmi-icon" aria-hidden="true">＋</span> Add item here
										</button>
									{/if}
									{#if onArchiveColumn && colItems.length > 0}
										{#if onCreateInColumn}
											<div class="lane-menu-sep"></div>
										{/if}
										{#if confirmArchiveColumn === colValue}
											<div class="lane-menu-confirm">
												<span>Archive {colItems.length} item{colItems.length === 1 ? '' : 's'}?</span>
												<div class="lmc-actions">
													<button class="lmc-yes" onclick={(e) => { e.stopPropagation(); onArchiveColumn?.(colItems); closeMenu(); }}>Archive</button>
													<button class="lmc-no" onclick={(e) => { e.stopPropagation(); confirmArchiveColumn = null; }}>Cancel</button>
												</div>
											</div>
										{:else}
											<button
												class="lane-menu-item lmi-danger"
												role="menuitem"
												onclick={(e) => { e.stopPropagation(); confirmArchiveColumn = colValue; }}
											>
												<span class="lmi-icon" aria-hidden="true">🗃</span> Archive all ({colItems.length})
											</button>
										{/if}
									{/if}
								</div>
							{/if}
						</div>
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
					delayTouchStart: touchDragDelayMs,
					// Disable item DnD whenever the parent has
					// requested rank-preserving order (search
					// active) — otherwise a drag would persist the
					// relevance-ranked subset order as the stored
					// `sort_order`. TASK-1367 / Codex R5. Also disable
					// under any non-manual page sort (TASK-1670): the
					// lane is comparator-ordered, so a drag couldn't
					// stick anyway.
					dragDisabled: isMobile || !canEdit || preserveOrder || sortMode !== 'manual'
				}}
				onconsider={(e) => handleConsider(colValue, e)}
				onfinalize={(e) => handleFinalize(colValue, e)}
				oncontextmenu={(e) => e.preventDefault()}
			>
				{#each colItems as item (item.id)}
					<div class="card-wrapper" class:no-drag={isMobile}>
						<ItemCard
							{item}
							{collection}
							compact={true}
							focused={focusedItemId === item.id}
							statusOptions={columns}
							onStatusClick={onStatusChange}
							progress={itemProgress?.[item.id] ?? null}
							{progressLabel}
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
{/if}

<style>
	.board-view {
		display: flex;
		gap: var(--space-4);
		flex: 1;
		min-height: 0;
		overflow-x: auto;
	}

	.kanban-column {
		display: flex;
		flex-direction: column;
		flex: 1 0 0;
		min-width: 220px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
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
		padding: var(--space-3) var(--space-4);
		border-bottom: 2px solid var(--text-secondary);
		border-radius: var(--radius-lg) var(--radius-lg) 0 0;
		font-weight: 700;
		font-size: 0.9em;
		cursor: grab;
		flex-shrink: 0;
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

	/* Lane-header affordances (TASK-1671): a `+` add-into-lane button and
	   a ⋯ kebab that opens the lane menu. Unlike the old hover-only
	   archive button these are always visible with real (≥28px, ≥32px on
	   touch) tap targets. */
	.lane-btn {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		min-width: 28px;
		height: 28px;
		padding: 0 4px;
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 1em;
		line-height: 1;
		cursor: pointer;
		border-radius: var(--radius-sm);
		transition: color 0.15s, background 0.15s;
	}

	.lane-add-btn {
		font-size: 1.15em;
		font-weight: 600;
	}

	.lane-btn:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
	}

	.lane-menu-wrap {
		position: relative;
		display: inline-flex;
	}

	.lane-menu {
		position: absolute;
		top: calc(100% + 4px);
		right: 0;
		z-index: 20;
		min-width: 180px;
		padding: var(--space-1);
		background: var(--bg-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		box-shadow: var(--shadow-md, 0 4px 12px rgba(0, 0, 0, 0.15));
		display: flex;
		flex-direction: column;
		gap: 2px;
	}

	.lane-menu-item {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		width: 100%;
		padding: 8px 10px;
		background: none;
		border: none;
		border-radius: var(--radius-sm);
		color: var(--text-primary);
		font-size: 0.875em;
		text-align: left;
		cursor: pointer;
	}

	.lane-menu-item:hover {
		background: var(--bg-hover);
	}

	.lane-menu-item.lmi-danger {
		color: var(--accent-red, #ef4444);
	}

	.lane-menu-item.lmi-danger:hover {
		background: color-mix(in srgb, var(--accent-red, #ef4444) 10%, transparent);
	}

	.lmi-icon {
		width: 1.1em;
		text-align: center;
		flex-shrink: 0;
	}

	.lane-menu-sep {
		height: 1px;
		margin: 2px 0;
		background: var(--border);
	}

	.lane-menu-confirm {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		padding: 8px 10px;
		font-size: 0.8125em;
		color: var(--text-secondary);
	}

	.lmc-actions {
		display: flex;
		gap: var(--space-2);
	}

	.lmc-yes {
		flex: 1;
		padding: 6px 10px;
		background: var(--accent-red, #ef4444);
		border: none;
		border-radius: var(--radius-sm);
		color: #fff;
		font-size: 0.8125em;
		cursor: pointer;
	}

	.lmc-no {
		flex: 1;
		padding: 6px 10px;
		background: var(--bg-tertiary);
		border: none;
		border-radius: var(--radius-sm);
		color: var(--text-primary);
		font-size: 0.8125em;
		cursor: pointer;
	}

	@media (max-width: 768px) {
		.lane-btn {
			min-width: 32px;
			height: 32px;
		}
	}

	.column-cards {
		display: flex;
		flex-direction: column;
		flex: 1;
		gap: var(--space-2);
		padding: var(--space-2);
		transition: background 0.15s ease;
		overflow-y: auto;
		min-height: 0;
	}

	.column-cards:global(.drop-target) {
		background: color-mix(in srgb, var(--accent-blue) 6%, transparent);
	}

	.card-wrapper {
		cursor: grab;
		-webkit-touch-callout: none;
		-webkit-user-select: none;
		user-select: none;
		/*
		 * Virtualization (TASK-1347 / PLAN-1343 Phase 1) — mirrors the
		 * approach landed for ListView in TASK-1346. The browser skips
		 * layout, style, and paint work for off-screen cards while
		 * leaving every wrapper mounted, so:
		 *   - svelte-dnd-action keeps every drop target in the tree
		 *     (drag-between-columns + drop-into-collapsed-section both
		 *     rely on the wrapper being present for hit-testing)
		 *   - column horizontal scroll + column reorder operate on
		 *     `.kanban-column`, which is unaffected by per-card paint
		 *     skipping
		 *   - keyboard focus on an off-screen card still finds the node
		 *     via querySelector and scrollIntoView rehydrates paint
		 *
		 * `.column-cards` is the scrolling ancestor here (overflow-y:
		 * auto), so content-visibility's near-viewport check uses the
		 * column as its frame — exactly what per-column virtualization
		 * needs. `contain-intrinsic-size: auto 80px` is slightly taller
		 * than the ListView placeholder because board cards render in
		 * compact mode with status + tags stacked, and the `auto`
		 * keyword caches the real measured height after first paint so
		 * later scrolls back to that card don't reflow.
		 *
		 * `overflow-clip-margin: 12px` is the fix for ItemCard's
		 * `.pr-badge`, which positions itself at `right: -6px` and
		 * deliberately protrudes past the card's right edge. CSS
		 * Containment L2 §3.4 / §4 specifies that `content-visibility:
		 * auto` applies paint containment continuously — including
		 * on-screen — and paint containment clips ink overflow. Without
		 * `overflow-clip-margin`, the badge would be clipped flush at
		 * the wrapper's content box. The margin budget:
		 *   - 6px for the badge's outward offset (`right: -6px`)
		 *   - ~3px for the badge's `box-shadow: 0 1px 3px` blur radius
		 *   - ~2px for the hover `transform: scale(1.05)` growth at
		 *     the typical 25-40px badge width
		 * 12px covers all three with a small safety margin. Codex
		 * rounds 2 + 3 on PR #489 walked through the spec misreading
		 * in round 1 and then the under-sized margin in round 2.
		 *
		 * Browser support for `overflow-clip-margin`: Chrome 90+,
		 * Firefox 102+, Safari 16.4+ — all browsers that ship
		 * `content-visibility: auto` already ship this. Older browsers
		 * ignore the property and fall back to the pre-virtualization
		 * (no-clip) behavior, which is also correct.
		 */
		content-visibility: auto;
		contain-intrinsic-size: auto 80px;
		overflow-clip-margin: 12px;
	}

	.card-wrapper:active {
		cursor: grabbing;
	}

	.card-wrapper.no-drag {
		cursor: default;
	}

	.card-wrapper.no-drag:active {
		cursor: default;
	}

	.column-empty {
		text-align: center;
		padding: var(--space-4);
		color: var(--text-muted);
		font-size: 0.82em;
	}

	@media (max-width: 768px) {
		.board-view {
			display: flex;
			overflow-x: auto;
			scroll-snap-type: x proximity;
			-webkit-overflow-scrolling: touch;
			gap: var(--space-3);
			padding: 0 var(--space-4) var(--space-3);
		}

		.kanban-column {
			min-width: 75vw;
			max-width: 75vw;
			scroll-snap-align: center;
			flex-shrink: 0;
		}

		.column-header {
			cursor: default;
		}

		.column-drag-handle {
			display: none;
		}
	}
</style>
