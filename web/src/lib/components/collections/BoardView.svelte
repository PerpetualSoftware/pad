<script lang="ts">
	import type { Item, Collection } from '$lib/types';
	import { parseSchema, parseFields } from '$lib/types';
	import { itemComparator, type SortMode } from '$lib/collections/itemSort';
	import { reorderGroup, disabledDirections, adjacentColumn, type ReorderDirection } from '$lib/collections/reorder';
	import { bucketByColumn, UNCATEGORIZED } from '$lib/collections/boardColumns';
	import { dndzone, TRIGGERS, SHADOW_ITEM_MARKER_PROPERTY_NAME } from 'svelte-dnd-action';
	import type { DndEvent } from 'svelte-dnd-action';
	import ItemCard from './ItemCard.svelte';
	import EmptyState from '../common/EmptyState.svelte';
	import LaneActionsMenu from './LaneActionsMenu.svelte';
	import { viewport } from '$lib/stores/breakpoint.svelte';


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
		 * Create an item in this lane from the inline draft card
		 * (TASK-1676), pre-filling the lane's group value. `navigate` true
		 * (Enter) opens the new item; false (nav-guard Save) just lands it
		 * in the lane. Throws on failure so the draft can be restored.
		 * Gated behind `canEdit`. When wired, the `+`/menu open a draft.
		 */
		onCreateInColumn?: (groupValue: string, title: string, navigate: boolean) => Promise<unknown> | void;
		/**
		 * Inline draft state (TASK-1676) — owned by the page (which holds
		 * the leave guard + dialog) so a draft survives a board↔list view
		 * switch that unmounts this component. Keyed by lane value:
		 * `draftText` is the in-progress title, `draftOpen` the card
		 * visibility. Bound.
		 */
		draftText?: Record<string, string>;
		draftOpen?: Record<string, boolean>;
		/**
		 * Bulk lane actions (TASK-1672), each operating on the lane's
		 * CURRENTLY-FILTERED items via the bulk endpoint. Surfaced in the
		 * ⋯ LaneActionsMenu. All canEdit-gated by the caller.
		 */
		onMoveColumn?: (items: Item[], status: string) => void;
		onTagColumn?: (items: Item[], tag: string) => void;
		onUntagColumn?: (items: Item[], tag: string) => void;
		onSetPriorityColumn?: (items: Item[], priority: string) => void;
		onAssignColumn?: (items: Item[], userId: string) => void;
		/** Workspace members (for "Assign all") and tag suggestions (for "Tag all"). */
		members?: { user_id: string; user_name?: string }[];
		tagSuggestions?: string[];
		/** True when a search/filter is narrowing the lanes — shown in menu labels. */
		filtered?: boolean;
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
		/**
		 * Opt-in split-pane open (PLAN-2105 / TASK-2111). Threaded straight
		 * through to each ItemCard; omitted everywhere except the collection
		 * page, so other surfaces keep full-page anchor navigation.
		 */
		onItemOpen?: (item: Item) => void;
		/**
		 * Report the board's rendered column structure (visual column order +
		 * within-column sort, as actually rendered) up to the parent so its
		 * keyboard navigation follows the real render order rather than a
		 * re-derived grouping that could drift from it (PLAN-2105 / TASK-2119).
		 * The parent (collection page) is the only caller.
		 */
		onColumnsRendered?: (columns: { value: string; items: Item[] }[]) => void;
	}

	let { items, collection, wsSlug = '', groupField = 'status', focusedItemId = null, onStatusChange, onReorder, onArchiveColumn, onGroupReorder, oncreate, onCreateInColumn, onMoveColumn, onTagColumn, onUntagColumn, onSetPriorityColumn, onAssignColumn, members = [], tagSuggestions = [], filtered = false, itemProgress, progressLabel = 'tasks', canEdit = true, preserveOrder = false, sortMode = 'manual', draftText = $bindable({}), draftOpen = $bindable({}), onItemOpen, onColumnsRendered }: Props = $props();

	// Local — disables the draft card while its Enter-create is in flight.
	let savingDraft = $state(false);

	// Which lane's ⋯ menu is open (null = none). One menu open at a time;
	// the LaneActionsMenu component owns the drill-down + confirm state.
	let openMenuColumn = $state<string | null>(null);

	function toggleMenu(colValue: string) {
		openMenuColumn = openMenuColumn === colValue ? null : colValue;
	}

	function closeMenu() {
		openMenuColumn = null;
	}

	// Ephemeral per-lane sort overrides (TASK-1673): a lane sorts by its
	// override when set, else the page-wide `sortMode`. Not persisted —
	// cleared on reload. Available to everyone (sort is a view preference).
	let laneSortOverrides = $state<Record<string, SortMode>>({});
	function setLaneSort(colValue: string, mode: SortMode | null) {
		if (mode === null) {
			delete laneSortOverrides[colValue];
		} else {
			laneSortOverrides[colValue] = mode;
		}
	}
	const laneSortFor = (colValue: string): SortMode => laneSortOverrides[colValue] ?? sortMode;

	// ── Inline draft cards (TASK-1676) ──────────────────────────────────
	// Trello/GitHub-style: the `+` opens an editable draft card in the
	// lane; no item exists until Enter. The draft STATE lives on the page
	// (bound here) so it survives a board↔list view switch (which unmounts
	// this component) — the page also owns the leave guard + dialog. Blur
	// keeps the card; Escape hides it but retains text (page state).
	let draftInputs: Record<string, HTMLTextAreaElement | undefined> = {};

	function openDraft(col: string) {
		if (!onCreateInColumn) return;
		draftOpen[col] = true;
		requestAnimationFrame(() => draftInputs[col]?.focus());
	}

	// Escape hides the card but KEEPS the text so reopening restores it.
	function escapeDraft(col: string) {
		draftOpen[col] = false;
	}

	async function submitDraft(col: string) {
		const title = (draftText[col] ?? '').trim();
		if (!title || savingDraft || !onCreateInColumn) return;
		savingDraft = true;
		// Clear optimistically BEFORE the create navigates, so the page's
		// leave guard (fires when onCreateInColumn opens the new item)
		// doesn't re-flag this lane.
		const prior = draftText[col];
		delete draftText[col];
		draftOpen[col] = false;
		try {
			await onCreateInColumn(col, title, true);
		} catch {
			// Restore the draft on failure (the page toasts the error).
			draftText[col] = prior;
			draftOpen[col] = true;
		} finally {
			savingDraft = false;
		}
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
		// The pinned UNCATEGORIZED lane isn't a reorder target — a real column
		// can't be moved to its left (DR-1), so don't show a drop indicator on it.
		if (!draggedColumn || draggedColumn === colValue || colValue === UNCATEGORIZED) return;
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
		// Bucket items into their lanes, routing empty/unknown-value items
		// into the UNCATEGORIZED ('') lane instead of dropping them (IDEA-2275).
		const result = bucketByColumn(items, groupField, columns);
		// `preserveOrder` opts out of the in-column sort so search rank
		// from the parent isn't overridden — TASK-1367. Otherwise sort
		// each lane by its effective mode — the per-lane override if set,
		// else the page-wide sort (TASK-1670 / TASK-1673); 'manual'
		// resolves to the stored sort_order, preserving prior behavior.
		if (!preserveOrder) {
			for (const key of Object.keys(result)) {
				result[key].sort(itemComparator(laneSortFor(key), collection));
			}
		}
		return result;
	});

	// Cooldown after a drop — suppress syncs while reorder API calls + SSE events settle
	let dropCooldown = $state(false);

	// Whether the UNCATEGORIZED lane is shown — true when any item lacks a
	// valid group value (IDEA-2275: "only if needed"). Synced from the settled
	// prop data in the SAME gate as columnData so the lane doesn't vanish
	// mid-drag when its last card leaves. Written here, only ever READ in the
	// `renderColumns` derivation below — never read inside this effect
	// (CONVE-1688).
	let showUncategorized = $state(false);

	$effect(() => {
		const data = propColumnData;
		if (!isDragging && !dropCooldown) {
			columnData = data;
			showUncategorized = (data[UNCATEGORIZED]?.length ?? 0) > 0;
		}
	});

	// The lanes to render, left→right: the pinned UNCATEGORIZED lane first when
	// needed (DR-1), then the user-orderable real columns. UNCATEGORIZED is
	// deliberately kept OUT of `columnOrder` (the persisted, drag-reorderable
	// set) so it can't be reordered into the middle or written to saved order.
	let renderColumns = $derived(
		showUncategorized ? [UNCATEGORIZED, ...columnOrder] : columnOrder
	);

	// Surface the board's rendered column structure to the parent for keyboard
	// navigation (PLAN-2105 / TASK-2119). Visual column order (`columnOrder`,
	// which reflects any local column-drag) × each lane's rendered item order
	// (`columnData`, which reflects per-lane sort overrides), with DnD shadow
	// placeholders filtered so mid-drag nav never targets a phantom card. This
	// is the single source of truth — the parent navigates THIS, never a
	// re-derived grouping that could disagree with what's on screen.
	let navColumns = $derived(
		renderColumns.map((col) => ({
			value: col,
			items: (columnData[col] ?? []).filter((i: any) => !i[SHADOW_ITEM_MARKER_PROPERTY_NAME]),
		}))
	);
	$effect(() => {
		onColumnsRendered?.(navColumns);
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

		const { id: itemId, trigger } = e.detail.info;
		isDragging = false;

		// The zone that RECEIVED the card owns the group-value change + the
		// target-lane reindex; delegate that to the shared commit path,
		// passing the dnd-provided drop order as the placement.
		if (trigger === TRIGGERS.DROPPED_INTO_ZONE) {
			const originalItem = items.find((i) => i.id === itemId);
			if (originalItem) {
				await commitColumnMove(originalItem, columnValue, e.detail.items);
				return;
			}
		}

		// Source/other zone (the card left this lane) or the item is gone:
		// no status change — just re-densify this lane's remaining order.
		dropCooldown = true;
		if (onReorder) {
			const reorderUpdates = e.detail.items
				.filter((i: any) => !i[SHADOW_ITEM_MARKER_PROPERTY_NAME])
				.map((item, index) => ({ slug: item.id, sort_order: index }));
			if (reorderUpdates.length > 0) onReorder(reorderUpdates);
		}
		setTimeout(() => { dropCooldown = false; }, 2000);
	}

	// Shared commit tail for a card that changes columns — used by both drag
	// (handleFinalize's receiving zone) and the menu's Move left/right
	// (moveItem). Guards with the drop cooldown, changes the group value via
	// the page's status handler, persists the target lane's sort_order, then
	// releases the cooldown after SSE settles — or reverts the optimistic
	// order on failure. `placement` stays parametric: drag passes the
	// dnd-provided target order (e.detail.items); the menu passes 'top' (the
	// card has already been inserted at the target lane's head). DR-7.
	async function commitColumnMove(item: Item, targetColumn: string, placement: Item[] | 'top') {
		dropCooldown = true;

		let moveSucceeded = true;
		const fields = parseFields(item);
		if (fields[groupField] !== targetColumn) {
			try {
				await onStatusChange(item, targetColumn);
			} catch {
				moveSucceeded = false;
			}
		}

		if (moveSucceeded) {
			// Only persist reorder after a successful move.
			if (onReorder) {
				const order = placement === 'top' ? (columnData[targetColumn] ?? []) : placement;
				const reorderUpdates = order
					.filter((i: any) => !i[SHADOW_ITEM_MARKER_PROPERTY_NAME])
					.map((it, index) => ({ slug: it.id, sort_order: index }));
				if (reorderUpdates.length > 0) onReorder(reorderUpdates);
			}
			// Let SSE events settle before re-syncing from props.
			setTimeout(() => { dropCooldown = false; }, 2000);
		} else {
			// Move failed — immediately restore original positions.
			dropCooldown = false;
		}
	}

	function formatLabel(value: string): string {
		if (!value) return 'Uncategorized';
		return value.replace(/_/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase());
	}

	// Menu-driven reorder (IDEA-1898), lane-relative — the non-drag
	// counterpart for touch (board drag is disabled on mobile) and long
	// lanes. Scope is the item's own lane, matching the drag handler.
	function reorderItem(columnValue: string, item: Item, dir: ReorderDirection) {
		if (!onReorder) return;
		const grp = (columnData[columnValue] ?? []).filter(
			(i: any) => !i[SHADOW_ITEM_MARKER_PROPERTY_NAME]
		);
		const updates = reorderGroup(grp, item.id, dir);
		if (updates.length > 0) {
			onReorder(updates.map((u) => ({ slug: u.item.id, sort_order: u.sort_order })));
		}
	}

	// Menu-driven adjacent-column move (TASK-1908) — the horizontal
	// counterpart to reorderItem. Sets the item's group field to the
	// neighbouring column's value and lands the card at the TOP of that lane,
	// reusing the drag commit path (commitColumnMove). Because a menu move —
	// unlike a drag — doesn't get source-lane removal for free from the dnd
	// library, we optimistically pull the card out of the source lane and
	// insert it at the target lane's head BEFORE committing; otherwise the
	// card would render in BOTH lanes until the cooldown/SSE settle (DR-7).
	function moveItem(columnValue: string, item: Item, dir: 'left' | 'right') {
		// Adjacency follows the RENDER order (which includes the pinned
		// UNCATEGORIZED lane), so a card can move right out of it into the
		// first real column, or left into it — the menu counterpart to a drag.
		const target = adjacentColumn(renderColumns, columnValue, dir);
		if (target === null) return;

		const source = (columnData[columnValue] ?? []).filter((i) => i.id !== item.id);
		const dest = [item, ...(columnData[target] ?? []).filter((i) => i.id !== item.id)];
		columnData[columnValue] = source;
		columnData[target] = dest;

		commitColumnMove(item, target, 'top');
	}

	// Per-lane gate: edit permission, not search-preserving order, and the
	// lane is in manual sort (its override, else page sort). Deliberately
	// NOT gated on isMobile — unlike drag (disabled on mobile), the menu IS
	// the mobile reorder mechanism.
	function canReorderLane(columnValue: string): boolean {
		return canEdit && !preserveOrder && laneSortFor(columnValue) === 'manual';
	}

	// Combined hidden-direction set for a card's reorder menu: the vertical
	// edges (disabledDirections) plus the horizontal edges — hide `left` in
	// the first column, `right` in the last, so the edge option simply isn't
	// rendered (DR-1). Pure derivation from columnOrder — read in the
	// template, never a $state an $effect writes (CONVE-1688).
	function moveDisabledDirs(
		columnValue: string,
		index: number,
		length: number
	): Set<ReorderDirection | 'left' | 'right'> {
		const disabled: Set<ReorderDirection | 'left' | 'right'> = new Set(
			disabledDirections(index, length)
		);
		// Horizontal edges follow the RENDER order (incl. the pinned
		// UNCATEGORIZED lane) so the hidden left/right options match where
		// moveItem can actually land the card.
		const colIdx = renderColumns.indexOf(columnValue);
		if (colIdx <= 0) disabled.add('left');
		if (colIdx >= renderColumns.length - 1) disabled.add('right');
		return disabled;
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
	{#each renderColumns as colValue (colValue)}
		{@const colItems = columnData[colValue] ?? []}
		{@const isUncategorized = colValue === UNCATEGORIZED}
		{@const colDraggable = canEdit && !isUncategorized}
		<div
			class="kanban-column"
			class:drag-over-left={dragOverColumn === colValue}
			class:dragging-source={draggedColumn === colValue}
			class:uncategorized-column={isUncategorized}
			role="group"
			aria-label="{formatLabel(colValue)} column"
			ondragover={(e) => handleColumnDragOver(e, colValue)}
			ondragleave={handleColumnDragLeave}
			ondrop={(e) => handleColumnDrop(e, colValue)}
		>
			<!-- svelte-ignore a11y_no_static_element_interactions -->
			<div
				class="column-header {columnCssClass(colValue)}"
				draggable={colDraggable}
				role="toolbar"
				tabindex="0"
				ondragstart={colDraggable ? (e) => handleColumnDragStart(e, colValue) : undefined}
				ondragend={colDraggable ? handleColumnDragEnd : undefined}
			>
				{#if colDraggable}
					<span class="column-drag-handle" title="Drag to reorder">⠿</span>
				{/if}
				<span class="column-name">{formatLabel(colValue)}</span>
				<div class="column-actions">
					<span class="column-count">{colItems.length}</span>
					<!-- Affordance visibility is driven purely by callback
					     presence — each callback already encodes its own
					     permission (the `+` create is grant-aware; the bulk
					     verbs are owner/editor-gated by the page). Don't gate
					     on `canEdit`, or an owner/editor without a collection
					     edit grant (canBulkEdit true, canEdit false) couldn't
					     open the menu at all. TASK-1672 / Codex round 4. The
					     UNCATEGORIZED lane hides "add" (creating an explicitly
					     uncategorized item makes no sense) but keeps the bulk
					     ⋯ menu — "move/tag/assign all" is useful for triage. -->
					{#if onCreateInColumn && !isUncategorized}
						<button
							class="lane-btn lane-add-btn"
							title="Add item to {formatLabel(colValue).toLowerCase()}"
							aria-label="Add item to {formatLabel(colValue)}"
							onclick={() => openDraft(colValue)}
						>+</button>
					{/if}
					<!-- Kebab shows for create OR any non-empty lane (sort is
					     always available, even to viewers — TASK-1673). -->
					{#if onCreateInColumn || colItems.length > 0}
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
								<LaneActionsMenu
									items={colItems}
									groupValue={colValue}
									{groupField}
									{collection}
									{filtered}
									{members}
									{tagSuggestions}
									{sortMode}
									laneSort={laneSortOverrides[colValue]}
									onSetLaneSort={(m) => setLaneSort(colValue, m)}
									onClose={closeMenu}
									onAddItem={onCreateInColumn && !isUncategorized ? () => openDraft(colValue) : undefined}
									onArchive={onArchiveColumn ? () => onArchiveColumn?.(colItems) : undefined}
									onMove={onMoveColumn ? (status) => onMoveColumn?.(colItems, status) : undefined}
									onTag={onTagColumn ? (tag) => onTagColumn?.(colItems, tag) : undefined}
									onUntag={onUntagColumn ? (tag) => onUntagColumn?.(colItems, tag) : undefined}
									onSetPriority={onSetPriorityColumn ? (p) => onSetPriorityColumn?.(colItems, p) : undefined}
									onAssign={onAssignColumn ? (userId) => onAssignColumn?.(colItems, userId) : undefined}
								/>
							{/if}
						</div>
					{/if}
				</div>
			</div>
			{#if draftOpen[colValue]}
				<!-- Inline draft card (TASK-1676). Lives ABOVE the dndzone so
				     it isn't draggable and isn't a real item until saved. -->
				<div class="lane-draft">
					<textarea
						bind:this={draftInputs[colValue]}
						bind:value={draftText[colValue]}
						class="lane-draft-input"
						placeholder="Enter a title…"
						rows="2"
						disabled={savingDraft}
						onkeydown={(e) => {
							if (e.key === 'Enter' && !e.shiftKey) {
								e.preventDefault();
								submitDraft(colValue);
							} else if (e.key === 'Escape') {
								e.preventDefault();
								escapeDraft(colValue);
							}
						}}
					></textarea>
					<div class="lane-draft-actions">
						<button
							class="lane-draft-add"
							disabled={!draftText[colValue]?.trim() || savingDraft}
							onclick={() => submitDraft(colValue)}
						>Add card</button>
						<button class="lane-draft-close" onclick={() => escapeDraft(colValue)}>Close</button>
					</div>
				</div>
			{/if}
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
					dragDisabled: viewport.isMobile || !canEdit || preserveOrder || laneSortFor(colValue) !== 'manual'
				}}
				onconsider={(e) => handleConsider(colValue, e)}
				onfinalize={(e) => handleFinalize(colValue, e)}
				oncontextmenu={(e) => e.preventDefault()}
			>
				{#each colItems as item, i (item.id)}
					<div class="card-wrapper" class:no-drag={viewport.isMobile}>
						<ItemCard
							{item}
							{collection}
							compact={true}
							focused={focusedItemId === item.id}
							statusOptions={columns}
							onStatusClick={onStatusChange}
							progress={itemProgress?.[item.id] ?? null}
							{progressLabel}
							onReorderItem={canReorderLane(colValue) ? (it, dir) => reorderItem(colValue, it, dir) : undefined}
							onMoveItem={canReorderLane(colValue) ? (it, dir) => moveItem(colValue, it, dir) : undefined}
							horizontal={canReorderLane(colValue)}
							reorderDisabledDirs={canReorderLane(colValue) ? moveDisabledDirs(colValue, i, colItems.length) : undefined}
							{onItemOpen}
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
		/* Only the horizontal axis scrolls at this level — each .column-cards
		   owns its own vertical scroll. Without this, per CSS spec setting
		   overflow-x to a non-visible value promotes the visible overflow-y to
		   `auto`, so a 1px/scrollbar-height overflow of the stretched columns
		   spawns an unwanted vertical scrollbar and pushes the horizontal
		   scroll track below an empty band (the "oversized/detached" bottom
		   scrollbar in BUG-2127). Clipping any vertical overflow keeps the
		   horizontal scrollbar snug at the bottom of the columns. */
		overflow-y: hidden;
	}

	.kanban-column {
		display: flex;
		flex-direction: column;
		/* Lanes have a fixed 220px FLOOR and do NOT shrink (flex-shrink 0).
		   flex-grow 1 + basis 0 lets a few lanes GROW to fill a wide board, but
		   when the lanes don't all fit — many statuses, a narrow window, or the
		   detail pane open — they hold 220px each and the board SCROLLS
		   HORIZONTALLY (.board-view overflow-x: auto). That horizontal scroll is
		   the INTENDED behavior of a kanban board, NOT a bug.
		   DO NOT change this to flex-shrink 1 / min-width 0 to make the lanes
		   "fill" a narrow board — that removes the scroll and crams the lanes
		   (PR #946 / BUG-2127 did exactly that and it was reverted at the
		   owner's request). The mobile @media below uses fixed 75vw lanes. */
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
		flex: 0 0 auto;
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

	/* The pinned Uncategorized lane (IDEA-2275) — a triage bucket for items
	   with no valid group value. It isn't draggable/reorderable, so drop the
	   grab cursor, and a dashed muted accent distinguishes it from the real
	   status columns. */
	.uncategorized-column .column-header {
		cursor: default;
		border-bottom-style: dashed;
		border-bottom-color: var(--text-muted);
		color: var(--text-secondary);
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
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
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

	/* The lane menu panel + its drill-down styles live in
	   LaneActionsMenu.svelte (TASK-1672). */

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

	/* Inline draft card (TASK-1676). */
	.lane-draft {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		margin: var(--space-2) var(--space-2) 0;
		padding: var(--space-2);
		background: var(--bg-primary);
		border: 1px solid var(--accent-blue);
		border-radius: var(--radius-md);
		box-shadow: var(--shadow-sm, 0 1px 3px rgba(0, 0, 0, 0.12));
	}

	.lane-draft-input {
		width: 100%;
		resize: vertical;
		min-height: 2.4em;
		padding: var(--space-2);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		color: var(--text-primary);
		font-size: 0.875em;
		font-family: inherit;
		line-height: 1.4;
	}

	.lane-draft-actions {
		display: flex;
		gap: var(--space-2);
	}

	.lane-draft-add {
		padding: 5px 12px;
		background: var(--accent-blue);
		border: none;
		border-radius: var(--radius-sm);
		color: #fff;
		font-size: 0.8125em;
		font-weight: 600;
		cursor: pointer;
	}
	.lane-draft-add:disabled {
		opacity: 0.5;
		cursor: default;
	}

	.lane-draft-close {
		padding: 5px 10px;
		background: none;
		border: none;
		border-radius: var(--radius-sm);
		color: var(--text-muted);
		font-size: 0.8125em;
		cursor: pointer;
	}
	.lane-draft-close:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
	}

</style>
