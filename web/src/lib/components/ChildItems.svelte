<script lang="ts">
	import { SvelteMap } from 'svelte/reactivity';
	import { onDestroy, onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import { sseService } from '$lib/services/sse.svelte';
	import { syncService } from '$lib/services/sync.svelte';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { toastStore } from '$lib/stores/toast.svelte';
	import type { Item, Collection, PaneTarget } from '$lib/types';
	import { parseFields, parseSchema, formatItemRef } from '$lib/types';
	import { dndzone, TRIGGERS, SHADOW_ITEM_MARKER_PROPERTY_NAME } from 'svelte-dnd-action';
	import type { DndEvent } from 'svelte-dnd-action';
	import {
		reorderGroup,
		reorderedList,
		disabledDirections,
		type ReorderDirection
	} from '$lib/collections/reorder';
	import { shouldOpenInPane } from './collections/itemCardClick';
	import ItemActionsMenu from './collections/ItemActionsMenu.svelte';
	import ChildChart from './ChildChart.svelte';
	import NestedChildren from './NestedChildren.svelte';

	interface Props {
		wsSlug: string;
		username?: string;
		itemSlug: string;
		itemId: string;
		parentFields?: Record<string, any>;
		terminalStatuses?: string[];
		onChildrenChange?: (children: Item[]) => void;
		/**
		 * canEdit gates child reorder via drag (PLAN-1100 / TASK-1108).
		 * Default true preserves behavior for callers that don't pass it.
		 * Reorder is per-child mutation server-side; this is a zone-level
		 * proxy gate (svelte-dnd-action limitation, same as TASK-1106).
		 */
		canEdit?: boolean;
		/**
		 * PLAN-2154 Phase 2 / D2 / R12 (TASK-2172): master-freeze. When the
		 * full-page host peeks a pane beside this item's ItemDetail, the master
		 * passes `frozen={true}` so ALL ChildItems mutations (add/link-child +
		 * reorder) freeze while child-row NAVIGATION stays live. Kept SEPARATE
		 * from `canEdit` on purpose: add/link-child authorize off independent
		 * target-collection / source-child capabilities (NOT the parent's edit
		 * permission), so folding the freeze into `canEdit` would wrongly strip
		 * add-child from a non-peeking user who lacks parent-edit but has those
		 * capabilities. Defaults false → byte-identical for existing callers.
		 */
		frozen?: boolean;
		/**
		 * Instance-scoped shadow of the OWNING ItemDetail's own dirty/
		 * lastSaveTime (PLAN-2154 Phase 0 / R4, TASK-2156). ChildItems used to
		 * read the global `editorStore.dirty`/`lastSaveTime` singleton directly
		 * to skip reloading on a self-triggered content-save echo. With a
		 * second concurrently-mounted ItemDetail (the future full-page-host +
		 * pane, D2), that singleton is shared — one instance's save could
		 * suppress a DIFFERENT instance's ChildItems reload. Defaults preserve
		 * today's single-instance behavior for the (nonexistent) caller that
		 * doesn't pass them.
		 */
		selfDirty?: boolean;
		selfLastSaveTime?: number;
		/**
		 * In-pane drill interceptor (PLAN-2154 Architecture B.2 / TASK-2159).
		 * Threaded down from `ItemDetail`'s `fireOpenTarget` — `undefined` when
		 * no host wired a pane (e.g. the full-page route), in which case the
		 * child row's `<a href>` falls through to plain navigation. Forwarded
		 * recursively into `NestedChildren`.
		 */
		onOpenTarget?: (target: PaneTarget) => void;
	}

	let { wsSlug, username = '', itemSlug, itemId, parentFields, terminalStatuses, onChildrenChange, canEdit = true, frozen = false, selfDirty = false, selfLastSaveTime = 0, onOpenTarget }: Props = $props();

	const defaultTerminal = ['done', 'completed', 'resolved', 'cancelled', 'rejected', 'wontfix', 'fixed', 'implemented', 'archived', 'disabled', 'deprecated'];
	const terminal = $derived(terminalStatuses ?? defaultTerminal);

	let children = $state<Item[]>([]);
	let loading = $state(true);
	let error = $state('');
	let unsubscribeSSE: (() => void) | null = null;
	let unsubscribeSync: (() => void) | null = null;

	let expandedIds = $state<Set<string>>(new Set());

	function toggleExpand(child: Item) {
		const next = new Set(expandedIds);
		if (next.has(child.id)) {
			next.delete(child.id);
		} else {
			next.add(child.id);
		}
		expandedIds = next;
	}

	const statusOrder: string[] = ['in_progress', 'open', 'blocked', 'done'];
	const flipDurationMs = 200;
	const touchDragDelayMs = 500;

	let doneCount = $derived(children.filter((t) => terminal.includes(parseFields(t).status)).length);
	let totalCount = $derived(children.length);
	let percentage = $derived(totalCount > 0 ? Math.round((doneCount / totalCount) * 100) : 0);

	/** Set of child item IDs — exposed for deduplication by the parent page */
	export function getChildIds(): Set<string> {
		return new Set(children.map(c => c.id));
	}

	let groups = $derived.by(() => {
		const map = new SvelteMap<string, Item[]>();
		for (const child of children) {
			const status = parseFields(child).status ?? 'open';
			if (!map.has(status)) map.set(status, []);
			map.get(status)!.push(child);
		}
		const sorted: [string, Item[]][] = [];
		for (const s of statusOrder) {
			if (map.has(s)) sorted.push([s, map.get(s)!]);
		}
		for (const [s, items] of map) {
			if (!statusOrder.includes(s)) sorted.push([s, items]);
		}
		return sorted;
	});

	// ── Drag-and-drop state ──────────────────────────────────────────────────
	let isDragging = $state(false);
	let groupData: Record<string, Item[]> = $state({});

	$effect(() => {
		const g = groups;
		if (!isDragging) {
			const data: Record<string, Item[]> = {};
			for (const [status, statusChildren] of g) {
				data[status] = [...statusChildren];
			}
			groupData = data;
		}
	});

	function handleConsider(status: string, e: CustomEvent<DndEvent<Item>>) {
		groupData[status] = e.detail.items;
		if (!isDragging && e.detail.info.trigger === TRIGGERS.DRAG_STARTED) {
			if (typeof navigator !== 'undefined' && navigator.vibrate) {
				navigator.vibrate(50);
			}
		}
		isDragging = true;
	}

	async function handleFinalize(status: string, e: CustomEvent<DndEvent<Item>>) {
		groupData[status] = e.detail.items;
		isDragging = false;

		// HT-2176 Option A (TASK-2172): gate reorder INITIATION, not finalization.
		// A NEW drag can't START while frozen (`dragDisabled: !canEdit || frozen`
		// on the dnd zone), but a drag already IN PROGRESS when the pane opens must
		// finalize and persist — so do NOT reject on `frozen` here. `!canEdit`
		// stays (the zone gate mirror; a non-editor never reaches finalize anyway).
		if (!canEdit) return;

		const updates = groupData[status]
			.filter((i: any) => !i[SHADOW_ITEM_MARKER_PROPERTY_NAME])
			.map((item, index) => ({ id: item.id, sort_order: index }));

		try {
			for (const { id, sort_order } of updates) {
				// HT-2176 Option A (TASK-2172): NO per-PATCH freeze recheck. The
				// reorder was INITIATED before peeking (the top guard blocks a NEW
				// one); breaking mid-loop would persist it only partially, leaving
				// inconsistent sort_orders. Let the initiated reorder finish.
				await api.items.update(wsSlug, id, { sort_order });
			}
		} catch (e) {
			console.error('Failed to persist reorder:', e);
		}
	}

	// Menu-driven reorder (IDEA-1898) — the non-drag counterpart, scoped to
	// the child's status group. Unlike List/Board (which persist through the
	// page's optimistic local index), ChildItems owns its own `children`
	// state, so it updates the displayed group optimistically here and
	// persists the changed rows via the same per-child update loop the drag
	// path uses. A canonical reload (SSE/sync) settles it afterward.
	async function reorderChild(status: string, child: Item, dir: ReorderDirection) {
		// Freeze guard (TASK-2172 / R14): mirror handleFinalize. The kebab is
		// hidden while `!canEdit || frozen`; this drops a straggler invocation.
		if (!canEdit || frozen) return;
		const grp = (groupData[status] ?? []).filter(
			(i: any) => !i[SHADOW_ITEM_MARKER_PROPERTY_NAME]
		);
		const reordered = reorderedList(grp, child.id, dir);
		if (reordered === grp) return; // no-op (edge of group)

		// Optimistic: show the new order immediately with dense sort_order.
		groupData[status] = reordered.map((it, idx) => ({ ...it, sort_order: idx }));

		const updates = reorderGroup(grp, child.id, dir);
		try {
			for (const u of updates) {
				// Option A (TASK-2172): no per-PATCH freeze recheck — a reorder
				// initiated pre-pane finishes fully (see handleFinalize).
				await api.items.update(wsSlug, u.item.id, { sort_order: u.sort_order });
			}
		} catch (e) {
			console.error('Failed to persist reorder:', e);
		}
	}

	// ── Data loading ─────────────────────────────────────────────────────────

	// Monotonic fence so only the LATEST loadChildren() writes state. Multiple
	// callers race concurrently (mount effect, SSE, sync, and the post-mutation
	// refreshes below); without this a slower older fetch can resolve last and
	// clobber `children` with a stale snapshot — and push it through
	// onChildrenChange (Codex diff review). Plain `let` (CONVE-1688).
	let loadSeq = 0;

	async function loadChildren() {
		// Capture the request identity (item + workspace) + sequence BEFORE the
		// await. ItemDetail reuses this panel across a no-{#key} item switch (its
		// `itemSlug` prop just changes), so a slower A load must NOT overwrite
		// B's children — nor fire onChildrenChange with A's data into the
		// parent's childItemIds / progress overrides (PLAN-2105 / TASK-2112).
		const reqSlug = itemSlug;
		const reqWs = wsSlug;
		const seq = ++loadSeq;
		// `destroyed` guards the {#key itemSlug} teardown → same-slug remount
		// case: a late load from the old instance must not push stale children
		// through onChildrenChange into the freshly-mounted parent (Codex).
		const stale = () => destroyed || seq !== loadSeq || reqSlug !== itemSlug || reqWs !== wsSlug;
		loading = true;
		error = '';
		try {
			const loaded = await api.items.children(reqWs, reqSlug);
			if (stale()) return;
			children = loaded;
			onChildrenChange?.(children);
		} catch (err) {
			if (stale()) return;
			error = err instanceof Error ? err.message : 'Failed to load children';
			onChildrenChange?.([]);
		} finally {
			if (!stale()) loading = false;
		}
	}

	$effect(() => {
		void wsSlug;
		void itemSlug;
		loadChildren();
	});

	onMount(() => {
		unsubscribeSync = syncService.onSync((result) => {
			if (!wsSlug || !itemSlug) return;
			// Only reload children on actual changes, not when caught up
			if (result.type !== 'caught_up') {
				loadChildren();
			}
		});
	});

	$effect(() => {
		unsubscribeSSE?.();
		unsubscribeSSE = null;

		if (!wsSlug || !itemSlug) return;

		unsubscribeSSE = sseService.onItemEvent((event) => {
			if (!['item_created', 'item_updated', 'item_archived', 'item_restored'].includes(event.type)) return;
			// Skip self-triggered content saves — they don't affect children.
			// Reads the OWNING ItemDetail's per-instance selfDirty/
			// selfLastSaveTime props (TASK-2156), not the global
			// editorStore.dirty/lastSaveTime singleton — a concurrently-
			// mounted second ItemDetail's save must not suppress THIS
			// instance's reload (PLAN-2154 D2 / R4).
			if (event.type === 'item_updated' && (selfDirty || Date.now() - selfLastSaveTime < 5000)) return;
			loadChildren();
		});
	});

	onDestroy(() => {
		// DR-6b switch-safety fence. ChildItems is mounted inside a
		// `{#key itemSlug}` in ItemDetail, so an item switch DESTROYS this
		// instance rather than re-running its props — a captured-identity
		// compare can't detect that (a destroyed instance keeps its old
		// props). Every async add-child handler bails on this flag after its
		// await so a create/search/link in flight during an item switch
		// can't write into the wrong item or toast after teardown.
		destroyed = true;
		clearTimeout(searchDebounceTimer);
		unsubscribeSSE?.();
		unsubscribeSync?.();
	});

	function formatLabel(value: string): string {
		return value.replace(/_/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase());
	}

	// In-pane drill interception for a `.child-row` anchor (TASK-2159 /
	// PLAN-2154 Architecture B.2). Mirrors `ItemCard.handleCardClick`'s
	// bail-out order via the shared `shouldOpenInPane` predicate — modifier/
	// middle-click still falls through to the plain `<a href>` popout.
	function handleChildClick(e: MouseEvent, child: Item) {
		if (!shouldOpenInPane(e, !!onOpenTarget)) return;
		e.preventDefault();
		onOpenTarget?.({ ref: formatItemRef(child) ?? undefined, slug: child.slug, collectionSlug: child.collection_slug });
	}

	// ── Add child (PLAN-2140) ─────────────────────────────────────────────────
	// In-place "+ Add child" affordance with two independent modes:
	//   • Create new  — POST a fresh item with `fields.parent` = this item.
	//   • Link existing — reparent an existing item under this one via a
	//     `parent` link (single-parent replace + cycle-check server-side).
	// Create and Link are gated by INDEPENDENT capabilities (DR-2): create is
	// authorized by the target collection, link by the source child.

	// A collection defining an exact `parent` schema field would make the
	// server treat our `fields.parent` as a normal field instead of a
	// hierarchy edge (DR-1b) — exclude it. `plan` is evaluated independently
	// server-side and does NOT break `fields.parent`, so it is NOT excluded.
	function reservedParentKey(c: Collection): boolean {
		return parseSchema(c).fields.some((f) => f.key === 'parent');
	}

	// A collection with a required field lacking a default can't be created
	// from a title-only form (server rejects) — exclude it (DR-6a) so the
	// create picker only offers collections a bare title satisfies.
	function requiredNoDefault(c: Collection): boolean {
		return parseSchema(c).fields.some((f) => f.required && f.default == null);
	}

	let eligibleCollections = $derived(
		collectionStore.collections.filter(
			(c) =>
				!c.is_system &&
				workspaceStore.canEditCollection(c.id) &&
				!reservedParentKey(c) &&
				!requiredNoDefault(c)
		)
	);

	let createTabEnabled = $derived(eligibleCollections.length > 0);

	// DR-2: link is authorized by the SOURCE child, so any member who can
	// edit SOME item (role owner/editor, or an item/collection edit grant)
	// may reparent — there is no `workspaceStore.role`; read the membership.
	let canLinkExisting = $derived.by(() => {
		const m = workspaceStore.currentMembership;
		return (
			!!m &&
			(m.role === 'owner' ||
				m.role === 'editor' ||
				m.item_grants.some((g) => g.permission === 'edit') ||
				m.collection_grants.some((g) => g.permission === 'edit'))
		);
	});

	// Entry "+ Add child" shows iff at least one mode is usable — gated on the
	// INDEPENDENT target-collection / source-child capabilities (NOT the parent's
	// `canEdit`), so the freeze must not touch that logic. `&& !frozen` layers the
	// master-freeze on top without changing any non-peeking behavior (TASK-2172).
	let showAddChild = $derived((createTabEnabled || canLinkExisting) && !frozen);

	// ── Form state ─────────────────────────────────────────────────────────
	let addOpen = $state(false);
	// The user's SELECTED tab. `effectiveMode` maps it onto an actually-
	// enabled tab so a link-only user never lands on a disabled Create view
	// (DR-2). Computing the displayed tab as a derived — rather than
	// correcting `mode` inside an effect — avoids writing a $state an effect
	// also reads (CONVE-1688).
	let mode = $state<'create' | 'link'>('create');
	let effectiveMode = $derived.by((): 'create' | 'link' => {
		if (mode === 'create' && createTabEnabled) return 'create';
		if (mode === 'link' && canLinkExisting) return 'link';
		if (createTabEnabled) return 'create';
		if (canLinkExisting) return 'link';
		return 'create';
	});

	// Create mode
	let createCollSlug = $state('');
	let createTitle = $state('');
	let creating = $state(false);

	// Link mode
	let linkSearch = $state('');
	let linkResults = $state<Item[]>([]);
	let linkSearching = $state(false);
	let confirmCandidate = $state<Item | null>(null);
	let linking = $state(false);

	// DR-6b fences. Plain `let` (never `$state`): read/written only in
	// handlers + onDestroy, never in an effect that also reads them
	// (CONVE-1688). `destroyed` is set in onDestroy; `searchSeq` discards a
	// slow older search that resolves after a newer one.
	let destroyed = false;
	let searchSeq = 0;
	// Debounce the link-search network call so typing doesn't fire one request
	// per keystroke (rate-limit relief). Plain `let` timer per the CommandPalette
	// idiom; cleared on reset + onDestroy so a pending fetch never fires after
	// the form closes or the instance is torn down.
	const SEARCH_DEBOUNCE_MS = 250;
	let searchDebounceTimer: ReturnType<typeof setTimeout> | undefined;

	// Candidates: drop the parent itself, items already children here, and
	// any the caller can't edit (view-only → 403 on link) — DR-2.
	let linkCandidates = $derived(
		linkResults.filter(
			(r) =>
				r.id !== itemId &&
				!children.some((c) => c.id === r.id) &&
				workspaceStore.canEditItem(r)
		)
	);

	// DR-3: always confirm — a link can be a MOVE. Name the old parent when
	// it's visible (search omits lineage, so treat absence as "unknown").
	let confirmMessage = $derived.by(() => {
		const cand = confirmCandidate;
		if (!cand) return '';
		const ref = formatItemRef(cand) ?? cand.slug;
		if (cand.parent_ref && cand.parent_slug !== itemSlug) {
			return `${ref} is currently under ${cand.parent_ref} — move it here?`;
		}
		return `Add ${ref} as a child here? If it's already a child elsewhere it will be moved.`;
	});

	function defaultCollSlug(): string {
		const eligible = eligibleCollections;
		if (eligible.length === 0) return '';
		const tasks = eligible.find((c) => c.slug === 'tasks');
		if (tasks) return tasks.slug;
		// Parent's own collection, when it's eligible and we can identify it.
		const parent = collectionStore.activeItem;
		if (parent && parent.id === itemId) {
			const own = eligible.find((c) => c.id === parent.collection_id);
			if (own) return own.slug;
		}
		return eligible[0].slug;
	}

	function resetForm() {
		createTitle = '';
		linkSearch = '';
		linkResults = [];
		confirmCandidate = null;
		linkSearching = false;
		// Cancel a pending debounced search so it doesn't fire a wasted request
		// after the form was cleared/closed.
		clearTimeout(searchDebounceTimer);
		// Invalidate any in-flight search so its late result can't repopulate
		// linkResults after the form was cleared/closed (Codex diff review P1).
		searchSeq++;
	}

	// Close + clear the add-child form whenever the item identity changes.
	// ItemDetail keys ChildItems on itemSlug, so an itemSlug change already
	// remounts us with a fresh form — this additionally covers a same-slug
	// switch across workspaces (itemId/wsSlug change, itemSlug constant, no
	// remount) so a stale title/collection can't be submitted against the new
	// item (Codex diff review). Dedicated identity-trigger effect per CONVE-606;
	// reads only identity props and writes only form state (CONVE-1688-safe).
	$effect(() => {
		void itemId;
		void wsSlug;
		addOpen = false;
		resetForm();
	});

	function toggleAddChild() {
		if (addOpen) {
			addOpen = false;
			resetForm();
			return;
		}
		mode = createTabEnabled ? 'create' : 'link';
		createCollSlug = defaultCollSlug();
		resetForm();
		addOpen = true;
	}

	function selectMode(next: 'create' | 'link') {
		// DR-6c: mode switching is locked while a submit is in flight.
		if (creating || linking) return;
		if (next === 'create' && !createTabEnabled) return;
		if (next === 'link' && !canLinkExisting) return;
		mode = next;
		confirmCandidate = null;
		// Leaving Link mode hides the search box — cancel a pending debounced
		// search + invalidate any in-flight one so it can't fire/repopulate
		// after the input is gone (Codex review P2).
		clearTimeout(searchDebounceTimer);
		searchSeq++;
		linkSearching = false;
	}

	async function submitCreate() {
		// Freeze guard (TASK-2172): the form is hidden while frozen, but drop a
		// straggler (e.g. an Enter keydown mid-freeze) so no child is created.
		if (frozen) return;
		const title = createTitle.trim();
		const collSlug = createCollSlug;
		if (!title || !collSlug || creating) return;
		// DR-6b: capture identity BEFORE the await; bail after it if the
		// instance was destroyed (item switch) or the identity moved.
		const reqWs = wsSlug;
		const reqSlug = itemSlug;
		const reqId = itemId;
		const stale = () => destroyed || reqWs !== wsSlug || reqSlug !== itemSlug || reqId !== itemId;
		creating = true;
		try {
			// DR-1: parent goes ONLY through the `fields` JSON `parent` key.
			// NEVER send `parent_id` — that writes the denormalized column
			// without creating the child link.
			await api.items.create(reqWs, collSlug, {
				title,
				fields: JSON.stringify({ parent: reqId })
			});
			if (stale()) return;
			addOpen = false;
			resetForm();
			toastStore.show('Child added', 'success');
			// DR-4: refresh directly (SSE is suppressed while the editor is
			// dirty/recently saved), don't rely on the event.
			loadChildren();
		} catch (e: any) {
			if (stale()) return;
			const msg: string = e?.message ?? 'Failed to add child';
			if (typeof msg === 'string' && msg.includes('item created but parent link failed')) {
				// DR-5: the item committed but the parent edge failed. Surface
				// it and refresh; do NOT auto-retry (would orphan-duplicate).
				addOpen = false;
				resetForm();
				toastStore.show(msg, 'error');
				loadChildren();
			} else {
				// DR-6a: any other 400/error — keep the form open to retry.
				toastStore.show(msg, 'error');
			}
		} finally {
			// Busy flag is per-instance UI, not identity-scoped data (data
			// writes above are fenced by stale()). Clear it whenever the
			// request settles; a write to a destroyed instance is a no-op.
			creating = false;
		}
	}

	// Debounced input handler. Clearing/emptying the box takes effect
	// immediately (no wasted request); a non-empty query fires searchLink()
	// only after the user pauses for SEARCH_DEBOUNCE_MS.
	function onSearchInput() {
		clearTimeout(searchDebounceTimer);
		if (!linkSearch.trim()) {
			// Invalidate any in-flight query so its late result can't repopulate
			// results after the box was cleared (searchSeq fence).
			searchSeq++;
			linkResults = [];
			linkSearching = false;
			return;
		}
		// Query changed: invalidate any in-flight search (its result is now
		// stale) and drop the previous results so they aren't shown or clickable
		// during the debounce window; show loading until the new search lands
		// (Codex review P2).
		searchSeq++;
		linkResults = [];
		linkSearching = true;
		searchDebounceTimer = setTimeout(() => searchLink(), SEARCH_DEBOUNCE_MS);
	}

	async function searchLink() {
		const q = linkSearch.trim();
		if (!q) {
			// Bump the sequence so any in-flight query is invalidated when the
			// input is cleared — its late result must not repopulate results
			// (Codex diff review P1).
			searchSeq++;
			linkResults = [];
			linkSearching = false;
			return;
		}
		// DR-6b: per-query sequence + identity fence.
		const seq = ++searchSeq;
		const reqWs = wsSlug;
		const reqSlug = itemSlug;
		const reqId = itemId;
		const stale = () =>
			destroyed || seq !== searchSeq || reqWs !== wsSlug || reqSlug !== itemSlug || reqId !== itemId;
		linkSearching = true;
		try {
			const res = await api.search(q, { workspace: reqWs });
			if (stale()) return;
			linkResults = (res.results || []).map((r) => r.item);
		} catch {
			if (stale()) return;
			linkResults = [];
		} finally {
			// Only the latest query controls the spinner (seq guard); a stale
			// query settling must not clear a newer query's spinner.
			if (seq === searchSeq) linkSearching = false;
		}
	}

	function openConfirm(cand: Item) {
		if (linking) return;
		confirmCandidate = cand;
	}

	async function confirmLink() {
		const cand = confirmCandidate;
		// Freeze guard (TASK-2172): mirror submitCreate — no reparent while frozen.
		if (!cand || linking || frozen) return;
		// DR-6b: capture identity BEFORE the await.
		const reqWs = wsSlug;
		const reqSlug = itemSlug;
		const reqId = itemId;
		const stale = () => destroyed || reqWs !== wsSlug || reqSlug !== itemSlug || reqId !== itemId;
		linking = true;
		try {
			// SOURCE = candidate, TARGET = current item id. Address the source
			// by stable `id` (ResolveItem tries UUID first) — not the mutable
			// slug, which could be reclaimed by another item mid-confirm (Codex).
			// The server does single-parent replace + cycle detection.
			await api.links.create(reqWs, cand.id, {
				target_id: reqId,
				link_type: 'parent'
			});
			if (stale()) return;
			addOpen = false;
			resetForm();
			toastStore.show('Child linked', 'success');
			loadChildren(); // DR-4
		} catch (e: any) {
			if (stale()) return;
			// Cycle / conflict → toast, keep the form usable (stay on confirm).
			toastStore.show(e?.message ?? 'Failed to link child', 'error');
		} finally {
			// Per-instance UI flag (data writes above are stale()-fenced);
			// clear whenever the request settles.
			linking = false;
		}
	}
</script>

{#if loading || error || children.length > 0 || showAddChild}
<div class="child-items">
	<div class="section-header">
		<h3>Children</h3>
		<div class="section-header-controls">
			{#if totalCount > 0}
				<span class="child-count">{doneCount}/{totalCount} done</span>
			{/if}
			{#if showAddChild}
				<button
					class="add-child-toggle"
					onclick={toggleAddChild}
					disabled={creating || linking}
					aria-expanded={addOpen}
				>
					{addOpen ? 'Cancel' : '+ Add child'}
				</button>
			{/if}
		</div>
	</div>

	<!-- `&& !frozen` unmounts an already-open add-child form the instant the
	     parent freezes (TASK-2172): the master-freeze must leave no live mutation
	     controls, not just block their writes. -->
	{#if addOpen && !frozen}
		<div class="add-child-form">
			<div class="add-child-tabs" role="tablist">
				<button
					class="add-child-tab"
					class:active={effectiveMode === 'create'}
					role="tab"
					aria-selected={effectiveMode === 'create'}
					disabled={!createTabEnabled || creating || linking}
					onclick={() => selectMode('create')}
				>
					Create new
				</button>
				<button
					class="add-child-tab"
					class:active={effectiveMode === 'link'}
					role="tab"
					aria-selected={effectiveMode === 'link'}
					disabled={!canLinkExisting || creating || linking}
					onclick={() => selectMode('link')}
				>
					Link existing
				</button>
			</div>

			{#if effectiveMode === 'create'}
				<div class="add-child-body add-child-create">
					<select
						class="add-child-select"
						bind:value={createCollSlug}
						disabled={creating}
						aria-label="Collection"
					>
						{#each eligibleCollections as coll (coll.id)}
							<option value={coll.slug}>{coll.name}</option>
						{/each}
					</select>
					<input
						class="add-child-input"
						type="text"
						placeholder="Child title…"
						bind:value={createTitle}
						disabled={creating}
						onkeydown={(e) => {
							if (e.key === 'Enter') {
								e.preventDefault();
								submitCreate();
							}
						}}
					/>
					<button
						class="add-child-submit"
						disabled={creating || !createTitle.trim() || !createCollSlug}
						onclick={submitCreate}
					>
						{creating ? 'Adding…' : 'Add'}
					</button>
				</div>
			{:else}
				<div class="add-child-body add-child-link">
					{#if confirmCandidate}
						<div class="add-child-confirm">
							<p class="add-child-confirm-msg">{confirmMessage}</p>
							<div class="add-child-confirm-actions">
								<button
									class="add-child-submit"
									disabled={linking}
									onclick={confirmLink}
								>
									{linking ? 'Linking…' : 'Add as child'}
								</button>
								<button
									class="add-child-cancel"
									disabled={linking}
									onclick={() => (confirmCandidate = null)}
								>
									Back
								</button>
							</div>
						</div>
					{:else}
						<input
							class="add-child-input"
							type="text"
							placeholder="Search items to link…"
							bind:value={linkSearch}
							disabled={linking}
							oninput={onSearchInput}
						/>
						{#if linkSearching}
							<div class="add-child-hint">Searching…</div>
						{:else if linkCandidates.length > 0}
							<ul class="add-child-results">
								{#each linkCandidates as cand (cand.id)}
									<li>
										<button
											class="add-child-result"
											disabled={linking}
											onclick={() => openConfirm(cand)}
										>
											<span class="add-child-result-ref">{formatItemRef(cand) ?? ''}</span>
											<span class="add-child-result-title">{cand.title}</span>
										</button>
									</li>
								{/each}
							</ul>
						{:else if linkSearch.trim()}
							<div class="add-child-hint">No matching items.</div>
						{/if}
					{/if}
				</div>
			{/if}
		</div>
	{/if}

	{#if children.length > 0}
		<div class="progress-bar">
			<div class="progress-fill" style:width="{percentage}%"></div>
		</div>
	{/if}

	{#if !loading && children.length >= 2}
		<ChildChart {children} startDate={parentFields?.start_date} endDate={parentFields?.end_date} {terminalStatuses} />
	{/if}

	{#if loading}
		<div class="loading">
			<span class="spinner"></span>
			<span>Loading children...</span>
		</div>
	{:else if error}
		<div class="error-msg">{error}</div>
	{:else}
		{#each groups as [status, _statusChildren] (status)}
			<div class="child-group">
				<div class="group-label">{formatLabel(status)} ({(groupData[status] ?? []).length})</div>
				<div
					class="child-list"
					use:dndzone={{
						items: groupData[status] ?? [],
						flipDurationMs,
						type: 'child-item',
						dropTargetClasses: ['drop-target'],
						delayTouchStart: touchDragDelayMs,
						dragDisabled: !canEdit || frozen
					}}
					onconsider={(e) => handleConsider(status, e)}
					onfinalize={(e) => handleFinalize(status, e)}
				>
					{#each groupData[status] ?? [] as child, i (child.id)}
						{@const fields = parseFields(child)}
						{@const isDone = terminal.includes(fields.status)}
						{@const isExpanded = expandedIds.has(child.id)}
						{@const canExpand = child.has_children}
						<div class="child-item-wrapper">
							<div class="child-row-container">
								{#if canExpand}
									<button class="expand-toggle" onclick={(e) => { e.preventDefault(); toggleExpand(child); }} title={isExpanded ? 'Collapse' : 'Expand'}>
										<span class="expand-icon" class:expanded={isExpanded}>▸</span>
									</button>
								{/if}
								<a href="/{username}/{wsSlug}/{child.collection_slug}/{child.slug}" class="child-row" class:has-toggle={canExpand} onclick={(e) => handleChildClick(e, child)}>
									<span class="child-ref">{formatItemRef(child) ?? ''}</span>
									<span class="child-title" class:done={isDone}>{child.title}</span>
									{#if fields.priority}
										<span
											class="child-priority"
											class:high={fields.priority === 'high'}
											class:critical={fields.priority === 'critical'}
										>
											{fields.priority}
										</span>
									{/if}
								</a>
								{#if canEdit && !frozen}
									<ItemActionsMenu
										item={child}
										label={child.title}
										disabledDirs={disabledDirections(i, (groupData[status] ?? []).length)}
										onReorder={(dir) => reorderChild(status, child, dir)}
									/>
								{/if}
							</div>
							{#if canExpand && isExpanded}
								<NestedChildren {wsSlug} {username} parentSlug={child.slug} depth={1} maxDepth={3} {terminalStatuses} {onOpenTarget} />
							{/if}
						</div>
					{/each}
				</div>
			</div>
		{/each}

	{/if}
</div>

<!-- Print-only flat checklist (PLAN-620 / TASK-624). Hidden on screen;
     visible in print via @media print rule below. The interactive
     `.child-items` view is hidden in print so this takes its place. -->
{#if !loading && !error && children.length > 0}
	<div class="print-children" aria-hidden="true">
		<div class="print-children-header">
			Children ({doneCount}/{totalCount} done)
		</div>
		<ul class="print-child-list">
			{#each children as child (child.id)}
				{@const childFields = parseFields(child)}
				{@const isDone = terminal.includes(childFields.status)}
				<li class="print-child-row" class:done={isDone}>
					<span class="print-check">{isDone ? '[x]' : '[ ]'}</span>
					{#if formatItemRef(child)}
						<span class="print-child-ref">{formatItemRef(child)}</span>
					{/if}
					<span class="print-child-title">{child.title}</span>
					{#if childFields.status}
						<span class="print-child-status">({formatLabel(childFields.status)})</span>
					{/if}
				</li>
			{/each}
		</ul>
	</div>
{/if}
{/if}

<style>
	.child-items {
		padding: var(--space-4) 0;
		border-top: 1px solid var(--border);
	}

	.section-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		margin-bottom: var(--space-3);
	}

	.section-header h3 {
		margin: 0;
		font-size: 0.95em;
		font-weight: 600;
		color: var(--text-primary);
	}

	.child-count {
		font-size: 0.8em;
		color: var(--text-muted);
		font-weight: 400;
	}

	.progress-bar {
		height: 6px;
		background: var(--bg-tertiary);
		border-radius: 3px;
		overflow: hidden;
		margin-bottom: var(--space-3);
	}

	.progress-fill {
		height: 100%;
		background: var(--accent-green);
		border-radius: 3px;
		transition: width 0.3s ease;
	}

	.child-group {
		margin-top: var(--space-3);
	}

	.group-label {
		font-size: 0.7em;
		font-weight: 500;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
		margin-bottom: var(--space-2);
	}

	.child-item-wrapper {
		/* container for row + nested children */
	}

	.child-row-container {
		display: flex;
		align-items: center;
	}

	.expand-toggle {
		background: none;
		border: none;
		cursor: pointer;
		padding: 0 2px;
		color: var(--text-muted);
		font-size: 0.8em;
		line-height: 1;
		flex-shrink: 0;
		width: 20px;
		text-align: center;
	}

	.expand-toggle:hover {
		color: var(--text-primary);
	}

	.expand-icon {
		display: inline-block;
		transition: transform 0.15s ease;
	}

	.expand-icon.expanded {
		transform: rotate(90deg);
	}

	.child-list {
		min-height: 4px;
	}

	:global(.drop-target) {
		outline: 2px dashed var(--accent-blue);
		outline-offset: -2px;
		border-radius: var(--radius-sm);
	}

	.child-row-container :global(.item-actions-menu) {
		padding: 0 var(--space-1);
	}

	.child-row {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-2) var(--space-2);
		text-decoration: none;
		color: inherit;
		border-bottom: 1px solid var(--border);
		transition: background 0.1s;
		cursor: grab;
		-webkit-touch-callout: none;
		-webkit-user-select: none;
		user-select: none;
		/* Fill the row so the reorder kebab (a sibling of this <a> in the
		   flex container) sits flush right. */
		flex: 1;
		min-width: 0;
	}

	.child-row:hover {
		background: var(--bg-hover);
	}

	.child-row:active {
		cursor: grabbing;
	}

	.child-item-wrapper:last-child .child-row {
		border-bottom: none;
	}

	.child-ref {
		font-family: var(--font-mono);
		font-size: 0.78em;
		color: var(--text-muted);
		white-space: nowrap;
		flex-shrink: 0;
	}

	.child-title {
		flex: 1;
		font-size: 0.88em;
		color: var(--text-primary);
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.child-title.done {
		text-decoration: line-through;
		color: var(--text-muted);
	}

	.child-priority {
		font-size: 0.72em;
		padding: 1px 6px;
		border-radius: 3px;
		white-space: nowrap;
		font-weight: 500;
		color: var(--text-muted);
		background: var(--bg-tertiary);
		flex-shrink: 0;
	}

	.child-priority.high {
		color: var(--accent-amber);
		background: color-mix(in srgb, var(--accent-amber) 15%, transparent);
	}

	.child-priority.critical {
		color: var(--accent-orange);
		background: color-mix(in srgb, var(--accent-orange) 15%, transparent);
	}

	.loading {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-4) 0;
		color: var(--text-muted);
		font-size: 0.9em;
		justify-content: center;
	}

	.spinner {
		width: 16px;
		height: 16px;
		border: 2px solid var(--border);
		border-top-color: var(--accent-blue);
		border-radius: 50%;
		animation: spin 0.6s linear infinite;
	}

	@keyframes spin {
		to {
			transform: rotate(360deg);
		}
	}

	.error-msg {
		padding: var(--space-2) var(--space-3);
		background: color-mix(in srgb, var(--accent-red, #ef4444) 12%, transparent);
		color: var(--accent-red, #ef4444);
		border-radius: var(--radius);
		font-size: 0.85em;
	}

	/* ── Add child (PLAN-2140) ───────────────────────────────────────────── */
	.section-header-controls {
		display: flex;
		align-items: center;
		gap: var(--space-3);
	}

	.add-child-toggle {
		background: none;
		border: none;
		padding: var(--space-1) var(--space-2);
		font-size: 0.8em;
		font-weight: 500;
		color: var(--accent-blue);
		border-radius: var(--radius-sm);
		cursor: pointer;
		white-space: nowrap;
	}

	.add-child-toggle:hover:not(:disabled) {
		background: var(--bg-hover);
	}

	.add-child-toggle:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}

	.add-child-form {
		margin-bottom: var(--space-3);
		padding: var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--bg-secondary);
	}

	.add-child-tabs {
		display: flex;
		gap: var(--space-1);
		margin-bottom: var(--space-3);
	}

	.add-child-tab {
		flex: 1;
		padding: var(--space-2);
		font-size: 0.8em;
		font-weight: 500;
		color: var(--text-muted);
		background: none;
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		cursor: pointer;
	}

	.add-child-tab.active {
		color: var(--text-primary);
		background: var(--bg-hover);
		border-color: var(--accent-blue);
	}

	.add-child-tab:disabled {
		opacity: 0.4;
		cursor: not-allowed;
	}

	.add-child-body {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}

	.add-child-create {
		flex-direction: row;
		flex-wrap: wrap;
		align-items: center;
	}

	.add-child-select,
	.add-child-input {
		padding: var(--space-2);
		font-size: 0.85em;
		color: var(--text-primary);
		background: var(--bg-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
	}

	.add-child-input {
		flex: 1;
		min-width: 0;
	}

	.add-child-select {
		flex-shrink: 0;
		max-width: 40%;
	}

	.add-child-input:disabled,
	.add-child-select:disabled {
		opacity: 0.6;
		cursor: not-allowed;
	}

	.add-child-submit {
		padding: var(--space-2) var(--space-3);
		font-size: 0.85em;
		font-weight: 500;
		color: #fff;
		background: var(--accent-blue);
		border: none;
		border-radius: var(--radius-sm);
		cursor: pointer;
		white-space: nowrap;
	}

	.add-child-submit:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}

	.add-child-cancel {
		padding: var(--space-2) var(--space-3);
		font-size: 0.85em;
		color: var(--text-muted);
		background: none;
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		cursor: pointer;
	}

	.add-child-cancel:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}

	.add-child-hint {
		font-size: 0.8em;
		color: var(--text-muted);
		padding: var(--space-1) 0;
	}

	.add-child-results {
		list-style: none;
		margin: 0;
		padding: 0;
		max-height: 220px;
		overflow-y: auto;
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
	}

	.add-child-result {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		width: 100%;
		padding: var(--space-2);
		text-align: left;
		background: none;
		border: none;
		border-bottom: 1px solid var(--border);
		cursor: pointer;
		color: inherit;
	}

	.add-child-results li:last-child .add-child-result {
		border-bottom: none;
	}

	.add-child-result:hover:not(:disabled) {
		background: var(--bg-hover);
	}

	.add-child-result:disabled {
		opacity: 0.6;
		cursor: not-allowed;
	}

	.add-child-result-ref {
		font-family: var(--font-mono);
		font-size: 0.78em;
		color: var(--text-muted);
		white-space: nowrap;
		flex-shrink: 0;
	}

	.add-child-result-title {
		flex: 1;
		font-size: 0.85em;
		color: var(--text-primary);
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.add-child-confirm-msg {
		margin: 0 0 var(--space-2) 0;
		font-size: 0.85em;
		color: var(--text-primary);
	}

	.add-child-confirm-actions {
		display: flex;
		gap: var(--space-2);
	}

	/* -----------------------------------------------------------------
	   Print-only flat checklist (PLAN-620 / TASK-624).
	   Hidden on screen. In print:
	     - hide the interactive `.child-items` view (chart, drag-drop
	       groups, expand toggles, progress bar) since those rely on
	       state and controls that have no meaning on paper;
	     - show a plain `<ul>` of children in a simple checkbox layout:
	         [x] TASK-621 · Title (done)
	         [ ] TASK-622 · Title (in progress)
	     - the block is `break-inside: avoid` where it fits so the list
	       doesn't split across pages.
	   ----------------------------------------------------------------- */
	.print-children {
		display: none;
	}

	@media print {
		.child-items {
			display: none !important;
		}

		.print-children {
			display: block;
			margin: 14pt 0 0 0;
			padding-top: 8pt;
			border-top: 1px solid #ccc;
			page-break-inside: avoid;
			break-inside: avoid;
		}
		.print-children-header {
			font-size: 10pt;
			font-weight: 600;
			text-transform: uppercase;
			letter-spacing: 0.05em;
			color: #333;
			margin: 0 0 6pt 0;
		}
		.print-child-list {
			list-style: none;
			padding: 0;
			margin: 0;
		}
		.print-child-row {
			font-size: 10pt;
			line-height: 1.45;
			padding: 1pt 0;
			color: #000;
			break-inside: avoid;
		}
		.print-check {
			display: inline-block;
			width: 15pt;
			font-family: var(--font-mono);
			color: #000;
			font-weight: 500;
		}
		.print-child-ref {
			font-weight: 500;
			margin-right: 4pt;
			color: #333;
			font-variant-numeric: tabular-nums;
		}
		.print-child-title {
			color: #000;
		}
		.print-child-row.done .print-child-title {
			color: #555;
		}
		.print-child-status {
			color: #777;
			margin-left: 4pt;
			font-size: 9pt;
			font-style: italic;
		}
	}

</style>
