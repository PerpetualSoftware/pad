<script lang="ts">
	import { relationGroupingRefusal, relationGroupingRefusalMessage } from '$lib/collections/relationGroups';
	import type { Item, Collection } from '$lib/types';
	import { getStatusOptions, parseSchema, parseFields } from '$lib/types';
	import { itemComparator, type SortMode } from '$lib/collections/itemSort';
	import { reorderGroup, disabledDirections, adjacentColumn, type ReorderDirection } from '$lib/collections/reorder';
	import {
		bucketByColumn,
		formatLaneLabel,
		laneKey,
		laneValue,
		UNCATEGORIZED,
	} from '$lib/collections/boardColumns';
	import {
		narrowRelationRow,
		relationLaneAcceptsDrop,
		relationLaneAriaName,
		relationLaneValueFor,
		relationLanes,
		type RelationLane,
	} from '$lib/collections/relationGroups';
	import { localIndex } from '$lib/stores/localIndex.svelte';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { columnAccentClassFor } from '$lib/utils/fieldColors';
	import { dndzone, TRIGGERS, SHADOW_ITEM_MARKER_PROPERTY_NAME } from 'svelte-dnd-action';
	import type { DndEvent } from 'svelte-dnd-action';
	import ItemCard from './ItemCard.svelte';
	import EmptyState from '../common/EmptyState.svelte';
	import LaneActionsMenu from './LaneActionsMenu.svelte';
	import { viewport } from '$lib/stores/breakpoint.svelte';
	import { draftKey, lostLaneLabel, type DraftSaveTarget } from '$lib/collections/laneDrafts';


	interface Props {
		items: Item[];
		collection: Collection;
		wsSlug?: string;
		groupField?: string;
		focusedItemId?: string | null;
		/**
		 * A DROP INTO A LANE — "put this item in this lane", which names the GROUP
		 * field. Distinct from `onStatusChange` below, which names `status`. They
		 * were one prop until BUG-3068: this component passed its single
		 * `onStatusChange` to BOTH the drop handler and the card's status chip, so
		 * on a board grouped by anything but `status` a chip click wrote into the
		 * group field.
		 */
		onLaneChange: (item: Item, laneValue: string) => void | Promise<void>;
		/**
		 * A STATUS CHIP CLICK — names the `status` field and nothing else, at every
		 * grouping. Optional, unlike the lane writer: a board without one simply
		 * shows a static chip, which is what every non-collection consumer of
		 * `ItemCard` already does.
		 */
		onStatusChange?: (item: Item, newStatus: string) => void | Promise<void>;
		onReorder?: (updates: { slug: string; sort_order: number }[]) => void;
		onArchiveColumn?: (items: Item[]) => void;
		onGroupReorder?: (newOrder: string[]) => void;
		oncreate?: () => void;
		/**
		 * Create an item from the inline draft card keyed `groupValue` — a
		 * `draftKey`, which the page resolves to a lane (TASK-1676, BUG-3214),
		 * pre-filling the lane's group value. Throws on failure
		 * so the draft can be restored. Gated behind `canEdit`. When wired,
		 * the `+`/menu open a draft.
		 *
		 * `reveal` reports INTENT, not a destination: true means "the user
		 * submitted this draft, show them the item", false means "create it
		 * quietly" (the page's nav-guard Save-all). What revealing does — and
		 * whether the viewport gets one at all — is the page's call, so this
		 * component stays unaware of the split pane (IDEA-2298).
		 */
		onCreateInColumn?: (groupValue: string, title: string, reveal: boolean) => Promise<unknown> | void;
		/**
		 * Inline draft state (TASK-1676) — owned by the page (which holds
		 * the leave guard + dialog) so a draft survives a board↔list view
		 * switch that unmounts this component. Keyed by `draftKey(groupField,
		 * lane)` — the field AND the lane, so a regroup cannot read one field's
		 * draft as another field's same-named lane (BUG-3214): `draftText` is the
		 * in-progress title, `draftOpen` the card visibility. Bound.
		 */
		draftText?: Record<string, string>;
		draftOpen?: Record<string, boolean>;
		/**
		 * Where each non-empty draft saves (BUG-3043), keyed like `draftText`.
		 * A `rehomed` draft's lane no longer exists: it renders inside the
		 * Uncategorized lane, marked with the lane it came from, and that lane
		 * is shown for it. Submitting it still passes its ORIGINAL key to
		 * `onCreateInColumn`, which is where the page resolves the target.
		 */
		draftPlacement?: Record<string, DraftSaveTarget>;
		/** Drafts that cannot be saved anywhere honest, with the notice naming the lost lane. */
		blockedDraftNotices?: { lane: string; message: string }[];
		/** Discard one draft (the way out of a blocked or re-homed one). */
		onDiscardDraft?: (lane: string) => void;
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
		 * canEdit gates drag-to-reorder, drag-to-lane-change (the drop that writes
		 * the GROUP field), and column reordering. See ListView.svelte for the
		 * rationale (zone-level gate).
		 *
		 * NOT the status CHIP, which is gated per ITEM inside `ItemCard` on
		 * `canEditItem` (BUG-3068 round 2) — this flag is `canEditCollection`, and
		 * item grants deliberately do not promote to it, so gating the chip here
		 * withheld it from a guest the server would have honoured.
		 *
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

	let { items, collection, wsSlug = '', groupField = 'status', focusedItemId = null, onLaneChange, onStatusChange, onReorder, onArchiveColumn, onGroupReorder, oncreate, onCreateInColumn, onMoveColumn, onTagColumn, onUntagColumn, onSetPriorityColumn, onAssignColumn, members = [], tagSuggestions = [], filtered = false, itemProgress, progressLabel = 'tasks', canEdit = true, preserveOrder = false, sortMode = 'manual', draftText = $bindable({}), draftOpen = $bindable({}), draftPlacement = {}, blockedDraftNotices = [], onDiscardDraft, onItemOpen, onColumnsRendered }: Props = $props();

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
	// Keyed by `laneKey(value)` (BUG-3208): a bare option value finds the
	// INHERITED member for every Object.prototype name, so a lane named
	// `constructor` read a function as its override, ignored the page sort and
	// lost its reorder menu.
	let laneSortOverrides = $state<Record<string, SortMode>>({});
	function setLaneSort(colValue: string, mode: SortMode | null) {
		if (mode === null) {
			delete laneSortOverrides[laneKey(colValue)];
		} else {
			laneSortOverrides[laneKey(colValue)] = mode;
		}
	}
	const laneSortFor = (colValue: string): SortMode =>
		laneSortOverrides[laneKey(colValue)] ?? sortMode;

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
		// The composer closes on submit on EVERY viewport (IDEA-2298) — on
		// desktop the pane takes focus anyway, so leaving it open for
		// Trello-style rapid entry would just fight the pane for the cursor.
		//
		// Clear the text optimistically, BEFORE the await, so the page's
		// unsaved-draft leave guard can't see this lane as dirty while the
		// create is in flight. The desktop reveal is a same-pathname `?item=`
		// push, which that guard skips regardless — belt and suspenders.
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
	// See viewport.dragDisabled (BUG-3158).
	const noTouchDrag = $derived(viewport.dragDisabled);

	let schema = $derived(parseSchema(collection));
	let field = $derived(schema.fields.find((f) => f.key === groupField));

	// GROUPING BY A RELATION (TASK-2998 / PLAN-2857 U7). Every other groupable
	// type has an option list to bucket against; a relation's lanes are the
	// targets the rows point at, so they are derived from the items and
	// resolved through the local index. The rules — ordering, what a deleted or
	// dangling target is labelled, which lanes accept a drop — live in
	// `$lib/collections/relationGroups`, because they are decisions and a
	// decision reachable only by mounting a board is one nobody tests.
	// A DECLARED TARGET IS REQUIRED (codex round 2). `narrowRelationRow` skips
	// the collection check when there is nothing to check against, so a legacy
	// or half-written relation field with no `collection` would resolve ids
	// ANYWHERE in the workspace and label lanes with whatever it found. The
	// filter UI already requires it; the board did not.
	let isRelationGroup = $derived(field?.type === 'relation' && !!field?.collection);
	/**
	 * Why grouping is refused for this field, or null (U4).
	 *
	 * A `multi_relation` is refused by lead ruling — one item belongs to as many
	 * lanes as it has references and `bucketByColumn`'s invariant is exactly one
	 * — and a relation with no declared target for the reason above.
	 *
	 * The board cannot be "ungrouped" the way the list can: a board IS lanes. So
	 * its fallback is a single UNCATEGORIZED lane — what changes is that it no
	 * longer does so SILENTLY. An Uncategorized lane with no explanation reads
	 * as "none of these items has a value", which is false and unactionable.
	 *
	 * "The lane it ALREADY produced for this case" is what this said until round
	 * 8, and it was not true: with `options` retained through a type change the
	 * board kept its named lanes and bucketed into them under the notice. The
	 * refusal now drives `columns` (below) rather than only the affordances.
	 */
	let groupingRefusal = $derived(relationGroupingRefusal(field));
	let knownCollectionSlugs = $derived(
		new Set(collectionStore.collections.map((c) => c.slug)),
	);
	let resolveRelation = $derived((id: string) =>
		wsSlug
			? narrowRelationRow(
					localIndex.findByIdOrSlug(wsSlug, id),
					id,
					field?.collection,
					knownCollectionSlugs,
				)
			: null,
	);
	let relationLaneList = $derived<RelationLane[]>(
		isRelationGroup ? relationLanes(items, groupField, resolveRelation) : [],
	);
	let relationLaneByValue = $derived(
		new Map(relationLaneList.map((lane) => [lane.value, lane])),
	);
	/**
	 * The board's lanes.
	 *
	 * NO lanes when grouping is refused (codex round 8, R8-3), which sends every
	 * item to UNCATEGORIZED and leaves exactly one lane on screen — the single
	 * lane the refusal notice describes.
	 *
	 * Round 7 gated every affordance that WRITES the group value and left the
	 * lanes themselves reading it, so the board went on bucketing under a notice
	 * saying it did not. That is the same false premise round 6 corrected one
	 * derivation below: "a multi_relation declares no options" is true of the
	 * schemas people write and enforced nowhere, so a `multi_select` retyped to
	 * `multi_relation` keeps `options` like `["A","A,B"]` — and a lane value is
	 * the STRINGIFIED array, which can match one. Items then landed in separate
	 * scalar-labelled lanes underneath "Showing everything ungrouped."
	 *
	 * One lane rather than keeping the notice and dropping it: the notice is the
	 * smaller change and it says something true, which is the point of it.
	 * IDEA-3034 holds the multi-lane alternative (one lane per reference, an
	 * item appearing in several), which is a feature rather than a repair.
	 *
	 * NOTHING forces the UNCATEGORIZED lane to appear here, which looks like a
	 * gap and is not. `showUncategorized` tracks that lane's contents, and under
	 * a refusal every item is in it — while a board with no items at all never
	 * reaches the lanes, since `items.length === 0` renders `EmptyState` in
	 * their place. A forcing term was written and then REMOVED after no mutant
	 * could reach it, rather than kept with an explanation attached.
	 */
	let columns = $derived(
		groupingRefusal
			? []
			: isRelationGroup
				? relationLaneList.map((lane) => lane.value)
				: (field?.options ?? []),
	);

	// Column order state — tracks the displayed order, syncs from schema when not dragging
	/**
	 * What a CARD's status picker offers (BUG-3157; it cycled before): the `status` field's DECLARED
	 * OPTIONS, at every grouping, read from the schema rather than from the
	 * board's lanes.
	 *
	 * It was `columns` — the lanes — and that was only ever right when the board
	 * was grouped BY status, where the two are the same array (`columns` is
	 * `field?.options` and `field` IS status). Grouped by anything else it
	 * produced a defect that does NOT look like the list's version of BUG-3068,
	 * which is why it needs saying: `ItemCard` cycled
	 * from `statusOptions.indexOf(fields.status)`, so on a board grouped by `priority`
	 * the item's STATUS was looked up in a list of PRIORITIES and normally missed,
	 * returning -1 and making the next index 0 — so the click jumped the card to
	 * the FIRST LANE from wherever it was, while the chip's label went on showing
	 * the status it was not changing. NOT every click: a status that happened to
	 * equal one of the lane values was found, and cycled within the lane
	 * vocabulary instead, which is wrong differently rather than less. The write
	 * was type-coherent either way (a lane value into the field those lanes come
	 * from), so BUG-3057's conversion accepted it and the user got a SUCCESS
	 * toast — the absent signal was an error, not a toast.
	 *
	 * The relation and refusal arms no longer empty this. They existed because
	 * the chip's callback wrote `fields[groupField]`, and this file's previous
	 * comment named the alternative it was declining — repointing the chip at the
	 * real status field, "a different feature and not one this unit was asked
	 * for". BUG-3068 is that unit: the chip's callback is now a separate prop
	 * naming `status`, the group field is unreachable from it, and a relation
	 * lane says nothing about whether an item's status can be written.
	 *
	 * THE RETAINED-OPTIONS HAZARD the round-6 note recorded is still real and is
	 * NOT closed by this line — reading the status field instead of the group
	 * field only moves it when the two differ, and a board grouped BY a `status`
	 * that was retyped keeps them the same field, stale `options` and all. What
	 * closes it is one layer down, in `ItemCard`, and it is TWO guards rather
	 * than one, each reachable on its own (both mutation-checked from
	 * `chipWritesStatus.svelte.test.ts`):
	 *
	 *   - the VALUE-shape test (`typeof fields.status === 'string'`) withholds a
	 *     retyped field whose stored value is a LIST — a `multi_select` status,
	 *     which no field-type test would catch since it is not a relation;
	 *   - `chippable` withholds a field whose declared TYPE is a relation
	 *     (BUG-3016) — the case the value test cannot see, because a scalar
	 *     relation's value IS a string.
	 *
	 * Named here rather than re-guarded: a third guard for the same hazard is how
	 * the round-5 mutant came to survive its own test.
	 */
	let cardStatusOptions = $derived(getStatusOptions(collection));

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
	/**
	 * Lanes keyed by `laneKey(value)`, never by the raw option value (BUG-3208).
	 * An option named `__proto__` re-parented a raw-keyed record instead of
	 * creating a lane, and a re-parented record is not proxied by `$state`, so
	 * the whole board lost its drag reactivity. Read and write it only through
	 * `laneKey`.
	 */
	let columnData: Record<string, Item[]> = $state({});

	let propColumnData = $derived.by(() => {
		// Bucket items into their lanes, routing empty/unknown-value items
		// into the UNCATEGORIZED ('') lane instead of dropping them (IDEA-2275).
		const buckets = bucketByColumn(
			items,
			groupField,
			columns,
			isRelationGroup ? (item) => relationLaneValueFor(item, groupField, resolveRelation) : undefined,
		);
		// `preserveOrder` opts out of the in-column sort so search rank
		// from the parent isn't overridden — TASK-1367. Otherwise sort
		// each lane by its effective mode — the per-lane override if set,
		// else the page-wide sort (TASK-1670 / TASK-1673); 'manual'
		// resolves to the stored sort_order, preserving prior behavior.
		if (!preserveOrder) {
			for (const [value, list] of buckets) {
				list.sort(itemComparator(laneSortFor(value), collection));
			}
		}
		const result: Record<string, Item[]> = {};
		for (const [value, list] of buckets) {
			result[laneKey(value)] = list;
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

	// The lane STRUCTURE the synced data was bucketed for (BUG-3042). The gate
	// below holds back a DATA re-sync while a drag or its post-drop cooldown is
	// running, so a card does not jump lanes mid-flight. It was also holding
	// back a change of structure (a regroup, or a schema retype that refuses
	// the grouping), which is not what it protects: `columnOrder` follows
	// `columns` at once, so the lanes switched while `columnData` stayed keyed
	// by the OLD ones, and the board showed its notice over zero cards until
	// the gate cleared. A structural change is a different view, not a sync,
	// so it goes through.
	//
	// The column list counts only where it comes from the SCHEMA (a select
	// field's options: an option removed in the window would otherwise hide
	// its cards). On a relation board the lanes are derived from the ITEMS, so
	// a drop that empties a lane would count as a structural change and force
	// exactly the mid-cooldown re-bucket the gate prevents.
	let laneStructure = $derived(
		[groupField, isRelationGroup, !!groupingRefusal, ...(isRelationGroup ? [] : columns)].join('\u0000')
	);
	let syncedStructure: string | null = null;

	$effect(() => {
		const data = propColumnData;
		const structure = laneStructure;
		if ((!isDragging && !dropCooldown) || structure !== syncedStructure) {
			syncedStructure = structure;
			columnData = data;
			showUncategorized = (data[laneKey(UNCATEGORIZED)]?.length ?? 0) > 0;
		}
	});

	// The lanes to render, left→right: the pinned UNCATEGORIZED lane first when
	// needed (DR-1), then the user-orderable real columns. UNCATEGORIZED is
	// deliberately kept OUT of `columnOrder` (the persisted, drag-reorderable
	// set) so it can't be reordered into the middle or written to saved order.
	/** A schema field's label by key, for a draft moved here by a regroup (BUG-3214). */
	function fieldLabelFor(fieldKey: string): string {
		return parseSchema(collection).fields.find((f) => f.key === fieldKey)?.label || fieldKey;
	}

	// Drafts whose lane is gone, shown inside Uncategorized (BUG-3043). That
	// lane is rendered for them even when no ITEM is in it: without this the
	// re-home would move the draft into a lane nobody can see, which is the
	// defect again under a different name.
	let rehomedDrafts = $derived(
		Object.entries(draftPlacement).flatMap(([lane, t]) =>
			t.kind === 'rehomed' ? [{ lane, lost: { lostLane: t.lostLane, lostField: t.lostField } }] : []
		)
	);
	let renderColumns = $derived(
		showUncategorized || rehomedDrafts.length > 0 ? [UNCATEGORIZED, ...columnOrder] : columnOrder
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
			items: (columnData[laneKey(col)] ?? []).filter((i: any) => !i[SHADOW_ITEM_MARKER_PROPERTY_NAME]),
		}))
	);
	$effect(() => {
		onColumnsRendered?.(navColumns);
	});

	function handleConsider(columnValue: string, e: CustomEvent<DndEvent<Item>>) {
		columnData[laneKey(columnValue)] = e.detail.items;
		if (!isDragging && e.detail.info.trigger === TRIGGERS.DRAG_STARTED) {
			if (typeof navigator !== 'undefined' && navigator.vibrate) {
				navigator.vibrate(50);
			}
		}
		isDragging = true;
	}

	async function handleFinalize(columnValue: string, e: CustomEvent<DndEvent<Item>>) {
		columnData[laneKey(columnValue)] = e.detail.items;

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
		// A RELATION LANE ONLY ACCEPTS A DROP WHEN ITS TARGET IS LIVE
		// (TASK-2998). Writing the value of the uncategorised, deleted or
		// unresolved lanes would either clear the field or write a reference
		// TASK-2878's validator refuses — so the card would move on screen and
		// the write would fail behind it, which is worse than not moving.
		if (isRelationGroup) {
			const lane = relationLaneByValue.get(targetColumn);
			if (!lane || !relationLaneAcceptsDrop(lane)) {
				// AND PUT THE CARD BACK (codex round 1, P1). By the time this
				// runs, svelte-dnd-action has already moved the card in
				// `columnData` — refusing the WRITE without undoing that leaves
				// the board showing a move that never happened, which is the
				// exact failure the refusal exists to avoid, one step later.
				//
				// Re-reading `propColumnData` is how the failure path below
				// reverts too: it is the props-derived truth, and the sync
				// effect is gated on the cooldown rather than owning the
				// restore. Assigning here rather than releasing a cooldown
				// nobody set is the same repair without the two-second window.
				//
				// COPIED rather than aliased — defensive, and NOT for the
				// reason rounds 6 and 7 gave. Both rounds argued that the sync
				// effect assigns the derived VALUE into `columnData`, so a
				// later `columnData[col] = ...` writes THROUGH to
				// `propColumnData`'s cached object and assigning it back
				// restores nothing. That premise is FALSE, measured rather than
				// argued: a `$state` assigned a `$derived`'s object is a deep
				// proxy whose property writes do not reach the derived's cache.
				// A probe over exactly this shape ($derived.by object → $state
				// → property write → read the derived) reported
				// `after mutation, derived.a=[1]`, the pre-mutation value. The
				// rendered tests agree from the other side: the round-6 menu
				// test survives the alias mutant, and the failure-exit test
				// below passes on the cooldown release alone while going red
				// when that release is removed.
				// So the copy buys nothing against aliasing; it stays only
				// because a restore path should not hand its caller an object
				// somebody else owns. Do not re-derive the aliasing story from
				// the shape of this code — it has now cost two review rounds.
				columnData = Object.fromEntries(
					Object.entries(propColumnData).map(([key, list]) => [key, [...list]]),
				);
				return;
			}
		}
		dropCooldown = true;

		let moveSucceeded = true;
		const fields = parseFields(item);
		// TRIMMED for a relation, as the list already was (codex round 6). A
		// legacy value of `" id-red "` is ALREADY in the `id-red` lane, and a
		// raw `!==` fired a pointless write for it — the same asymmetry between
		// these two views, in the other direction this time.
		// NORMALISED on both sides (BUG-3053, lead review of PR #1348). The raw
		// field value against a lane STRING makes an item already in its own lane
		// look like it moved: `0 !== '0'`, `false !== 'false'`, and the "move"
		// then writes the string '0' into a number field.
		//
		// LIVE ON MAIN, unlike ListView's copy of this: `bucketByColumn` already
		// normalised, so a 0-scored item has always been visible in lane '0' here
		// and has always been draggable. The list's version of this line was
		// unreachable until the item stopped being dropped from the view.
		const currentValue = isRelationGroup
			? relationLaneValueFor(item, groupField, resolveRelation)
			: laneValue(fields[groupField]);
		// A REFUSED grouping has no group value to change, so a drop inside its
		// single fallback lane is a REORDER and nothing else (U4, codex round
		// 1). Without this the multi_relation case took the scalar arm:
		// `currentValue` is the stored ARRAY, `targetColumn` is a lane string,
		// they are never equal, and every drop fired a write that would replace
		// the list with a scalar. The server refuses it, `moveSucceeded` goes
		// false, and the reorder is silently dropped — while the refusal notice
		// above renders correctly the entire time, which is what hid it.
		if (!groupingRefusal && currentValue !== targetColumn) {
			try {
				await onLaneChange(item, targetColumn);
			} catch {
				moveSucceeded = false;
			}
		}

		if (moveSucceeded) {
			// Only persist reorder after a successful move.
			if (onReorder) {
				const order = placement === 'top' ? (columnData[laneKey(targetColumn)] ?? []) : placement;
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


	// Menu-driven reorder (IDEA-1898), lane-relative — the non-drag
	// counterpart for touch (board drag is disabled on mobile) and long
	// lanes. Scope is the item's own lane, matching the drag handler.
	function reorderItem(columnValue: string, item: Item, dir: ReorderDirection) {
		if (!onReorder) return;
		const grp = (columnData[laneKey(columnValue)] ?? []).filter(
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

		const source = (columnData[laneKey(columnValue)] ?? []).filter((i) => i.id !== item.id);
		const dest = [item, ...(columnData[laneKey(target)] ?? []).filter((i) => i.id !== item.id)];
		columnData[laneKey(columnValue)] = source;
		columnData[laneKey(target)] = dest;

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

	// Delegates to the shared accent mapper (shareView.ts) so the in-app
	// board and the public-share fork can never drift again — this was the
	// FIFTH parallel status-color-ish implementation (PR #1023 Codex catch).
	// Bonus: terminal_options-aware, so custom "Shipped"-style lanes read
	// as done here too, matching public boards.
	function columnCssClass(value: string): string {
		return columnAccentClassFor(field, value);
	}
</script>

<svelte:window onclick={handleWindowClick} />

{#if items.length === 0}
	<EmptyState {collection} {wsSlug} {oncreate} />
{:else}
{#if groupingRefusal}
	<p class="grouping-refused" role="status">
		<strong>Not grouped by {field?.label || groupField}.</strong>
		{relationGroupingRefusalMessage(groupingRefusal)}
	</p>
{/if}
{#snippet draftCard(key: string, movedFrom: { lostLane: string; lostField?: string } | null)}
	<div class="lane-draft" class:lane-draft-rehomed={movedFrom !== null}>
		{#if movedFrom !== null}
			<p class="lane-draft-moved">Moved from {lostLaneLabel(movedFrom, fieldLabelFor)}, a lane that no longer exists</p>
		{/if}
		<textarea
			bind:this={draftInputs[key]}
			bind:value={draftText[key]}
			class="lane-draft-input"
			placeholder="Enter a title…"
			rows="2"
			disabled={savingDraft}
			onkeydown={(e) => {
				if (e.key === 'Enter' && !e.shiftKey) {
					e.preventDefault();
					submitDraft(key);
				} else if (e.key === 'Escape' && movedFrom === null) {
					e.preventDefault();
					escapeDraft(key);
				}
			}}
		></textarea>
		<div class="lane-draft-actions">
			<button
				class="lane-draft-add"
				disabled={!draftText[key]?.trim() || savingDraft}
				onclick={() => submitDraft(key)}
			>Add card</button>
			{#if movedFrom === null}
				<button class="lane-draft-close" onclick={() => escapeDraft(key)}>Close</button>
			{:else if onDiscardDraft}
				<button class="lane-draft-close" onclick={() => onDiscardDraft?.(key)}>Discard</button>
			{/if}
		</div>
	</div>
{/snippet}
{#each blockedDraftNotices as notice (notice.lane)}
	<!-- BUG-3043 edge 1: the draft is kept, not re-homed into a place that
	     would not hold it; this is where the user sees it and can let it go. -->
	<div class="board-draft-blocked" role="alert">
		<p>{notice.message}</p>
		<p class="board-draft-blocked-text">“{draftText[notice.lane]}”</p>
		{#if onDiscardDraft}
			<button class="lane-draft-close" onclick={() => onDiscardDraft?.(notice.lane)}>Discard draft</button>
		{/if}
	</div>
{/each}
<div class="board-view">
	{#each renderColumns as colValue (colValue)}
		{@const colItems = columnData[laneKey(colValue)] ?? []}
		{@const isUncategorized = colValue === UNCATEGORIZED}
		{@const relLane = relationLaneByValue.get(colValue)}
		{@const laneName = relLane ? (relLane.title ?? relLane.label) : formatLaneLabel(colValue)}
		{@const laneRef = relLane?.ref ?? null}
		<!--
			A relation lane is NOT column-draggable and offers no "+". Its order
			is alphabetical rather than schema-held, so a reorder would have
			nowhere to persist to and would snap back; and creating an item
			"into" a lane means writing a relation, which the picker owns
			(TASK-2998 keeps that to the drop gesture in this unit).
		-->
		{@const colDraggable = canEdit && !isUncategorized && !isRelationGroup}
		<div
			class="kanban-column"
			class:drag-over-left={dragOverColumn === colValue}
			class:dragging-source={draggedColumn === colValue}
			class:uncategorized-column={isUncategorized}
			role="group"
			aria-label="{relationLaneAriaName(relLane, formatLaneLabel(colValue))} column"
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
				<span class="column-name">
					{#if laneRef}<span class="lane-ref">{laneRef}</span>{/if}{laneName}
					{#if relLane?.state === 'deleted'}<span class="lane-note" title="This item has been deleted.">(deleted)</span>{/if}
				</span>
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
					<!--
						AND NOT WHEN GROUPING IS REFUSED (codex round 7). A
						multi_relation that retained `options` produces NAMED
						lanes, so neither `isUncategorized` nor `isRelationGroup`
						withheld this: creating in one sent the lane string as
						the relation value, which the write door refuses. Same
						class as the drag and the status chip — every affordance
						that writes the GROUP VALUE has to ask the refusal.
					-->
					{#if onCreateInColumn && !isUncategorized && !isRelationGroup && !groupingRefusal}
						<button
							class="lane-btn lane-add-btn"
							title="Add item to {formatLaneLabel(colValue).toLowerCase()}"
							aria-label="Add item to {formatLaneLabel(colValue)}"
							onclick={() => openDraft(draftKey(groupField, colValue))}
						>+</button>
					{/if}
					<!-- Kebab shows for create OR any non-empty lane (sort is
					     always available, even to viewers — TASK-1673). -->
					{#if onCreateInColumn || colItems.length > 0}
						<div class="lane-menu-wrap">
							<button
								class="lane-btn lane-menu-btn"
								title="Lane actions"
								aria-label="{relationLaneAriaName(relLane, formatLaneLabel(colValue))} lane actions"
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
									laneSort={laneSortOverrides[laneKey(colValue)]}
									onSetLaneSort={(m) => setLaneSort(colValue, m)}
									onClose={closeMenu}
									onAddItem={onCreateInColumn && !isUncategorized && !isRelationGroup && !groupingRefusal
										? () => openDraft(draftKey(groupField, colValue))
										: undefined}
									onArchive={onArchiveColumn ? () => onArchiveColumn?.(colItems) : undefined}
									onMove={/* The FIFTH affordance that writes the group value —
										round 7 enumerated four and missed this one (round 8).
										It offers `statusField.options` as destinations, so a
										`status` field retyped to `multi_relation` with its
										options retained offers lanes that do not exist and
										sends a SCALAR into a list field. */
									onMoveColumn && !isRelationGroup && !groupingRefusal
										? (status) => onMoveColumn?.(colItems, status)
										: undefined}
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
			{#if draftOpen[draftKey(groupField, colValue)]}
				<!-- Inline draft card (TASK-1676). Lives ABOVE the dndzone so
				     it isn't draggable and isn't a real item until saved. -->
				{@render draftCard(draftKey(groupField, colValue), null)}
			{/if}
			{#if isUncategorized && onCreateInColumn}
				<!-- Re-homed drafts (BUG-3043): their lane no longer exists. Always
				     rendered, not gated on `draftOpen` — there is no "+" left to
				     reopen one with. -->
				{#each rehomedDrafts as d (d.lane)}
					{@render draftCard(d.lane, d.lost)}
				{/each}
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
					// Touch drag is off by INPUT, not width (BUG-3158): a landscape
					// phone is wider than the mobile breakpoint, and a finger held
					// past delayTouchStart while scrolling picked the card up and
					// dropped it in another lane. The card menu's move stays.
					dragDisabled: noTouchDrag || !canEdit || preserveOrder || laneSortFor(colValue) !== 'manual'
				}}
				onconsider={(e) => handleConsider(colValue, e)}
				onfinalize={(e) => handleFinalize(colValue, e)}
				oncontextmenu={(e) => e.preventDefault()}
			>
				{#each colItems as item, i (item.id)}
					<div class="card-wrapper" class:no-drag={noTouchDrag}>
						<ItemCard
							{item}
							{collection}
							compact={true}
							focused={focusedItemId === item.id}
							statusOptions={cardStatusOptions}
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
					<div class="column-empty">No {formatLaneLabel(colValue).toLowerCase()} items</div>
				{/if}
			</div>
		</div>
	{/each}
</div>
{/if}

<style>
	/* U4: the grouping-refused notice, matching ListView's. Plain and inline
	   rather than a toast — it describes a standing property of the view's
	   configuration, not an event, so it must survive a reload. */
	.grouping-refused {
		margin: 0 0 0.75rem;
		padding: 0.5rem 0.75rem;
		border-radius: 6px;
		background: var(--surface-2, rgba(127, 127, 127, 0.1));
		color: var(--text-2, inherit);
		font-size: 0.85rem;
		line-height: 1.4;
	}

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

	.column-header.col-open {
		border-bottom-color: var(--status-blue);
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

	.lane-ref {
		font-family: var(--font-mono, ui-monospace, monospace);
		font-size: 0.85em;
		opacity: 0.7;
		margin-right: 0.35em;
	}

	.lane-note {
		font-size: 0.85em;
		opacity: 0.7;
		margin-left: 0.35em;
		font-style: italic;
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
		box-shadow: var(--shadow-sm);
	}

	.lane-draft-rehomed {
		border-style: dashed;
	}

	.lane-draft-moved {
		margin: 0;
		font-size: 0.75em;
		color: var(--text-secondary);
	}

	.board-draft-blocked {
		margin: 0 0 var(--space-3);
		padding: var(--space-2) var(--space-3);
		border: 1px solid var(--accent-red);
		border-radius: var(--radius-md);
		font-size: 0.8125em;
		color: var(--text-primary);
	}

	.board-draft-blocked p {
		margin: 0 0 var(--space-1);
	}

	.board-draft-blocked-text {
		color: var(--text-secondary);
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
