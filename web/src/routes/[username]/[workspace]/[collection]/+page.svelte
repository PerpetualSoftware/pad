<script lang="ts">
	import { page } from '$app/state';
	import { browser } from '$app/environment';
	import { goto, beforeNavigate } from '$app/navigation';
	import { api, PadApiError, isPlanLimitError, planLimitMessage } from '$lib/api/client';
	import type { BulkItemsRequest, Collection, Item, QuickAction, View, ViewConfig } from '$lib/types';
	import { parseSettings, parseFields, parseSchema, parseTags, getStatusOptions, itemUrlId, formatItemRef } from '$lib/types';
	import { plansProgressToMap, fetchCollectionProgress } from '$lib/collections/progressMerge';
	import BoardView from '$lib/components/collections/BoardView.svelte';
	import ListView from '$lib/components/collections/ListView.svelte';
	import TableView from '$lib/components/collections/TableView.svelte';
	import ItemDetail from '$lib/components/items/ItemDetail.svelte';
	import FilterBar from '$lib/components/collections/FilterBar.svelte';
	import QuickActionsMenu from '$lib/components/common/QuickActionsMenu.svelte';
	import BottomSheet from '$lib/components/common/BottomSheet.svelte';
	import { viewport } from '$lib/stores/breakpoint.svelte';
	import SSEStatusIndicator from '$lib/components/SSEStatusIndicator.svelte';
	import { onDestroy, onMount, untrack } from 'svelte';
	import { sseService } from '$lib/services/sse.svelte';
	import { syncService } from '$lib/services/sync.svelte';
	import { toastStore } from '$lib/stores/toast.svelte';
	import ShareDialog from '$lib/components/ShareDialog.svelte';
	import EditCollectionModal from '$lib/components/collections/EditCollectionModal.svelte';
	import Modal from '$lib/components/common/Modal.svelte';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import { titleStore } from '$lib/stores/title.svelte';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { localIndex } from '$lib/stores/localIndex.svelte';
	import { localSearch, parseSearchQuery } from '$lib/stores/localSearch.svelte';
	import { createScrollRestoration } from '$lib/scroll/restore.svelte';
	import { confirmOpenChildrenOrThrow, isOpenChildrenError } from '$lib/items/openChildrenError';
	import { SORT_OPTIONS, priorityField, type SortMode } from '$lib/collections/itemSort';
	import {
		UNPARENTED_FILTER_FIELD,
		buildUnparentedViewFilter,
		clearParentFilter,
		filtersSetParent,
		readUnparentedParam,
		resolveParentUnparentedMutex,
		unparentedConfirmedRestricted,
		unparentedEffective,
		viewHasUnparentedFilter,
	} from '$lib/collections/unparentedFilter';
	import { KNOWN_COLLECTION_URL_PARAMS, buildCollectionUrlParams } from '$lib/collections/paneUrlParams';

	type ViewMode = 'list' | 'board' | 'table';

	// `metaLoading` tracks the collection-metadata / saved-views /
	// members fetch. The overall `loading` indicator combines that
	// with the localIndex bootstrap state so the empty-state CTA
	// doesn't flash for non-empty collections while items are still
	// hydrating (Codex P2 round 1 of TASK-1357). Scroll restoration
	// also gates on `loading`, so this prevents the "attempt-and-fail"
	// case where filteredItems is briefly empty.
	let metaLoading = $state(true);
	let collection = $state<Collection | null>(null);
	// `metaError` distinguishes a TRANSIENT collection-metadata load
	// failure (network blip / 5xx / 429) from a genuine not-found
	// (BUG-2025). A genuine 404 (`PadApiError.code === 'not_found'`)
	// leaves `collection` null and `metaError` null so the template
	// renders the terminal "Collection not found" empty state. Any
	// other thrown error sets `metaError`, which the template renders
	// as an error + Retry state — mirroring the items branch's
	// `indexError`/`deltaSyncFailed` retry box — instead of masking a
	// live collection as deleted. Cleared at the top of every load.
	let metaError = $state<Error | null>(null);
	let viewMode = $state<ViewMode>('list');
	// Page-wide within-group sort (TASK-1670 / IDEA-1648). 'manual' is the
	// stored sort_order (drag order). Persisted per collection.
	let sortMode = $state<SortMode>('manual');
	let activeFilters = $state<Record<string, string>>({});
	// Multi-select tag filter (OR semantics). Tags live on the top-level
	// `tags` column, not in `fields` JSON, so they're tracked separately
	// from `activeFilters` and applied in their own `filteredItems` branch.
	let selectedTags = $state<string[]>([]);
	// "Unparented only" chip state (TASK-2099 / PLAN-2095). Tracked separately
	// from `activeFilters` — it isn't a schema field filter, it's a structural
	// predicate over the local-first projection's `is_unparented` bit, and it
	// requires the caller to be unrestricted (DR-2). This is raw user/URL/
	// saved-view INTENT; whether it actually takes effect always routes
	// through `unparentedEffective` against `unparentedMetadataAvailable`
	// below, so a restricted caller's carried-over intent never filters
	// anything or renders the chip.
	let unparentedFilter = $state(false);
	let searchQuery = $state('');
	let showArchived = $state(false);
	let itemProgress = $state<Record<string, { total: number; done: number; label?: string }>>({});
	let progressLabel = $state('tasks');
	// `relationLabels` maps plan id → plan title for the task-card
	// "relates to" badge. `$derived` so it picks up plans as they
	// hydrate through the local index — the previous one-shot fetch
	// in loadCollection raced the localIndex bootstrap on cold loads
	// and could leave the badge empty until a manual refresh (Codex
	// P3 round 1 of TASK-1357).
	let relationLabels = $derived.by(() => {
		if (!wsSlug || collSlug !== 'tasks') return {};
		const labels: Record<string, string> = {};
		for (const p of localIndex.getByCollection(wsSlug, 'plans')) {
			labels[p.id] = p.title;
		}
		return labels;
	});

	// Saved views state
	let savedViews = $state<View[]>([]);
	let activeViewId = $state<string | null>(null);
	let savingView = $state(false);
	let saveViewOpen = $state(false);
	let saveViewName = $state('');
	let saveViewInput = $state<HTMLInputElement>();

	let shareDialogOpen = $state(false);
	let editCollectionOpen = $state(false);
	let editCollectionSection = $state<'general' | 'fields' | 'display' | 'actions' | undefined>(undefined);
	let workspaceMembers = $state<{ user_id: string; role: string; user_name?: string; user_email?: string }[]>([]);
	let searchInputEl = $state<HTMLInputElement>();
	// `searchResultRank` is a Map<itemId, rank> where rank is the 0-indexed
	// position in the ranked result list. `null` means no search active.
	// Storing the rank (not just IDs) lets `filteredItems` sort matched
	// rows by relevance so the localSearch exact-ref hoist + boost tuning
	// from TASK-1367 actually surfaces in the UI — Codex round 2 caught
	// that a `Set`-only filter let `updated_at DESC` order override the
	// ranking.
	let searchResultRank = $state<Map<string, number> | null>(null);
	let searchTimeout: ReturnType<typeof setTimeout>;

	let wsSlug = $derived(page.params.workspace ?? '');
	let username = $derived(page.params.username ?? '');
	let collSlug = $derived(page.params.collection ?? '');

	// PLAN-2105 Phase 2 — split-pane URL state. `?item=<ref>` is the
	// single URL-derived source of truth for which item (if any) has its
	// detail pane open. Derived over `page` (from $app/state) so it reacts
	// to back/forward, `goto`, and any other query-state change WITHOUT
	// being routed through the load cycle — `loadUrlFilters` only runs on a
	// ws/collection/showArchived change, never on same-pathname query
	// edits, so pane state must be a standalone derived over `page.url`.
	// No pane component is mounted yet (TASK-2112); this is the URL-state
	// groundwork the pane will read.
	let openItemRef = $derived(page.url.searchParams.get('item'));

	// Reactive parse of the current search query — shared with the
	// search-dispatch effect below so it doesn't reparse per run.
	// TASK-1367.
	let parsedSearch = $derived(parseSearchQuery(searchQuery));

	// `items` is now a $derived view over the local-first read model
	// (localIndex, PLAN-1343 / TASK-1355-1356). The collection page no
	// longer calls /items-index on every navigation; bootstrap is
	// idempotent and fires once per workspace per session. Filter by
	// the `showArchived` toggle at read time — the store holds both
	// live and archived rows. Widen to `Item` by setting content=''
	// to match the existing prop type contract for view components;
	// the detail page rehydrates content on open.
	let items = $derived<Item[]>(
		wsSlug && collSlug
			? localIndex
					.getByCollection(wsSlug, collSlug, { includeArchived: showArchived })
					.map((row) => ({ ...row, content: '' }) as Item)
			: [],
	);

	// `loading` is true until BOTH the collection metadata fetch AND
	// the localIndex bootstrap have settled. Without this, the empty-
	// state CTA can flash for non-empty collections while items are
	// still hydrating, and scroll restore can be consumed against an
	// empty filteredItems list (Codex P2 round 1).
	let indexState = $derived(
		wsSlug ? localIndex.bootstrapStateFor(wsSlug) : 'ready',
	);
	let indexReady = $derived(indexState === 'ready');
	// `indexError` surfaces a cold-load failure (transient /items-index
	// failure with no cache to fall back on) AND the post-revoke
	// reset state (`deltaSync` resets on 401/403 — see Codex P2 round
	// 5 of TASK-1357 — and the bootstrap effect can't re-trigger on
	// the same wsSlug/userId pair, so we surface the error directly
	// when indexState rolls back to 'cold' after we'd been 'ready'
	// or 'loading'). The template renders an error banner with a
	// retry CTA instead of the misleading "No items yet" empty state
	// or a stuck-forever "Loading…" spinner.
	let indexError = $derived(indexState === 'error');
	// `deltaSyncFailed` lets the auth-error reset on /items-changes
	// surface as an error banner even when the local-index state
	// has rolled back to 'cold'. Cleared on every successful
	// deltaSync.
	let deltaSyncFailed = $state(false);
	let loading = $derived(
		!metaError && (metaLoading || (!indexReady && !indexError && !deltaSyncFailed)),
	);

	// Unparented-filter projection scope (TASK-2099 / PLAN-2095 DR-2). `true`
	// only once the local index has confirmed (via `includes_unparented_
	// metadata` on an index/delta response) that THIS caller is unrestricted.
	// `null` (unknown, pre-bootstrap) and `false` (restricted) both read as
	// unavailable — the chip stays hidden and the filter never applies until
	// this is a confirmed `true`.
	//
	// Deliberately NOT gated on `pendingResyncFor`: a warm-cache boot's
	// PERSISTED capability bit can be momentarily stale (e.g. an offline
	// permission downgrade not yet reconciled), but that's the SAME
	// staleness window every other locally-cached field on `Item` already
	// has under the local-first model (PLAN-1343) — nothing else on this
	// page blocks rendering on `pendingResync`, and any client-side
	// filtering during that window only ever operates on data this caller
	// was already shown before the (possible) downgrade, so it isn't a NEW
	// disclosure. `includesUnparentedMetadataFor` self-corrects reactively
	// the moment a resync installs a fresh value — see the effect below.
	// (Codex review round 2 gated this on `pendingResyncFor`; round 3 P1/P2
	// showed that blunt gate both destroyed legitimate intent mid-resync
	// and could wedge the chip hidden forever after a live permission
	// upgrade, since `pendingResync` is cleared only by `bootstrap()`'s own
	// reconcile loop — never by the page's separate `deltaSync()` calls
	// that also trigger a resync during a live session.)
	let unparentedMetadataAvailable = $derived(
		wsSlug ? localIndex.includesUnparentedMetadataFor(wsSlug) === true : false,
	);
	// The chip's EFFECTIVE state — intent AND availability. Everything that
	// actually filters/badges/persists reads this, not the raw intent flag,
	// so a restricted caller's carried-over `unparentedFilter=true` (from a
	// URL or saved view) never filters anything or counts as an active
	// filter (DR-2).
	let unparentedApplied = $derived(unparentedEffective(unparentedFilter, unparentedMetadataAvailable));
	// A NARROWER signal than `!unparentedMetadataAvailable`, used ONLY to
	// gate the destructive "physically clear a stuck intent" action below.
	// Requires a CONFIRMED restriction (`includesUnparentedMetadataFor ===
	// false`, not just "unavailable" — which also covers `null`/unknown and
	// a mid-resync window where the true answer just hasn't landed yet) AND
	// `!pendingResyncFor` (no resync in flight that could still flip the
	// answer). Without this narrower gate, clearing on mere unavailability
	// would permanently discard a legitimate URL/saved-view intent the
	// instant it loaded during ANY in-flight resync — even one that was
	// about to confirm the caller unrestricted a moment later (Codex review
	// round 3, P1).
	let isUnparentedConfirmedRestricted = $derived(
		wsSlug
			? unparentedConfirmedRestricted(
					localIndex.includesUnparentedMetadataFor(wsSlug) === false,
					localIndex.pendingResyncFor(wsSlug),
				)
			: false,
	);

	// True once `loadUrlFilters()` has actually run for the CURRENT route
	// (`loadCollection` flips it `true` right after that call, on the
	// success path only; see `loadCollection`). Reset by the route-change
	// effect immediately below. `$state`, not plain — the URL-sync effect
	// below must reactively re-run once this flips, since the load itself
	// is asynchronous and happens well after the route-change flush that
	// resets it.
	let urlFiltersLoaded = $state(false);

	// Reset per-route bookkeeping whenever wsSlug/collSlug change. This
	// effect is declared BEFORE the URL-sync effect below on purpose:
	// Svelte runs effects with no parent/child relationship in declaration
	// order when a shared dependency invalidates them in the same flush, so
	// placing the reset first guarantees it always lands before the sync
	// effect could read a stale value in that same tick (Codex review round
	// 6 — relying on `metaLoading` transitioning was an ordering-fragile
	// proxy for "this is a new route" and also missed the case where a
	// route's `loadCollection` call fails, so `loadUrlFilters()` never ran
	// but `metaLoading` still flips back to `false` in the `finally`).
	//
	//   - `lastSyncedUnparented`: a value carried over from a PREVIOUS
	//     collection could otherwise mask a genuine transition in the new
	//     one (Codex review round 5, P2) — see the sync effect's comment.
	//   - `urlFiltersLoaded`: starts `false` for every new route; only
	//     `loadCollection`'s own `loadUrlFilters()` call (async, run later)
	//     flips it back, which is what actually gates the sync effect below
	//     — not `metaLoading`.
	// Mirrors the existing `defaultViewApplied` reset effect further down.
	$effect(() => {
		void wsSlug;
		void collSlug;
		lastSyncedUnparented = false;
		urlFiltersLoaded = false;
	});

	// Restricted clearing (DR-2) AND URL honesty as the projection scope
	// resolves. Two things this effect keeps correct once `indexReady`:
	//
	//   1. Once restriction is CONFIRMED (`isUnparentedConfirmedRestricted`),
	//      physically clear a stuck intent flag (from a URL or saved view
	//      carrying the pseudo-filter) instead of leaving it dangling —
	//      otherwise it would silently no-op (via `unparentedApplied`) but
	//      could still get re-persisted into the URL/a new saved view the
	//      moment something else calls `updateUrlFilters()`.
	//   2. Re-sync the URL whenever `unparentedApplied` (the EFFECTIVE
	//      state) actually changes value, in EITHER direction. This covers
	//      not just the clear-on-downgrade case above but also: a default
	//      saved view applied before local-index hydration (gated only on
	//      `!metaLoading`, not `indexReady` — see `applyViewConfig`'s
	//      caller) writes the URL with `unparentedApplied` still false
	//      (metadata unknown); once metadata resolves `true` afterward,
	//      the on-screen list becomes correctly filtered but nothing had
	//      re-written the address bar to match — a copied/reloaded URL
	//      would silently drop the filter (Codex review round 1).
	//
	//   Note the two conditions are independent, NOT merged into one
	//   `unparentedApplied !== lastSyncedUnparented` check (Codex review
	//   round 2, P2): a restricted caller loading a URL that carries the
	//   pseudo-filter has `unparentedApplied === false` BOTH before and
	//   after clearing the stuck raw intent (it was never effective to
	//   begin with), so that diff alone would never fire — leaving the
	//   literal `$unparented=true` sitting in the address bar even though
	//   internal state is correctly clean. So: sync whenever we just
	//   cleared a confirmed-restricted stuck intent, in addition to syncing
	//   on an effective-value transition.
	// Plain (non-reactive) memo, not `$derived` — we only need last-observed
	// bookkeeping inside the effect below, not a tracked value. `false`
	// matches `unparentedApplied`'s guaranteed value at this point (intent
	// is only ever set after mount, via URL/view load or user interaction).
	let lastSyncedUnparented = false;
	$effect(() => {
		// Gate on `urlFiltersLoaded`, not `metaLoading` (Codex review round
		// 6): `metaLoading` flips back to `false` on BOTH a successful load
		// (after `loadUrlFilters()` ran) AND a failed one (where it never
		// ran) — and relying on it also implicitly assumed this effect
		// re-runs strictly after the load-triggering effect's synchronous
		// reset, which isn't a documented guarantee across effects declared
		// with a dependency this indirect. `urlFiltersLoaded` is the
		// precise, explicit signal for "this route's URL-derived filter
		// state is real," reset synchronously by the effect declared right
		// above (guaranteed to run first) and only set `true` by
		// `loadCollection` itself once `loadUrlFilters()` has genuinely run.
		if (!wsSlug || !indexReady || !urlFiltersLoaded) return;
		let needsUrlSync = false;
		if (unparentedFilter && isUnparentedConfirmedRestricted) {
			unparentedFilter = false;
			needsUrlSync = true;
		}
		if (unparentedApplied !== lastSyncedUnparented) {
			needsUrlSync = true;
		}
		lastSyncedUnparented = unparentedApplied;
		if (needsUrlSync) updateUrlFilters();
	});

	// Bootstrap the workspace on entry. Idempotent: if already 'ready'
	// (and no pendingResync), this is a no-op; if 'cold', it kicks off
	// the warm-IDB / cold-/items-index flow. Re-runs when the
	// workspace slug or the signed-in user changes so a user switch
	// in the same browser tab gets a fresh per-user cache.
	//
	// Then run a deltaSync regardless of bootstrap state. Once the
	// localIndex is `ready`, bootstrap() no-ops — but an item the
	// user created/updated elsewhere (item detail page, dashboard,
	// or another tab) while this collection was unmounted is still
	// catchable via /items-changes. Without this, returning to the
	// collection page after creating an item elsewhere could miss
	// the new row until the next SSE event arrives (Codex P1 round
	// 5 of TASK-1357).
	$effect(() => {
		if (!wsSlug) return;
		const uid = authStore.userId || null;
		(async () => {
			try {
				await localIndex.bootstrap(wsSlug, { userId: uid });
			} catch {
				// Bootstrap errors flip bootstrapState to 'error'; the
				// indexError-gated error banner surfaces them. 401/403
				// redirect/purge is handled by the API client +
				// TASK-1360.
			}
			// Catch up any deltas missed while the user was on a
			// different page within the same workspace.
			await deltaSync(wsSlug);
		})();
	});
	// isOwner now comes from workspaceStore (PLAN-1100 / TASK-1101) — populated
	// by workspaceStore.setCurrent via the /me endpoint. The workspaceMembers
	// array remains for the assignee dropdown / member rows.
	let isOwner = $derived(workspaceStore.isOwner);
	// Per-collection edit predicate (PLAN-1100 / TASK-1104). Drives create-item
	// affordances (+ New, quick-create, empty-state CTA) on this collection
	// page. Mirrors the server's per-collection edit cascade (item CRUD on a
	// collection requires owner / editor / collection grant edit / etc.).
	let canEditThisCollection = $derived(
		collection ? workspaceStore.canEditCollection(collection.id) : false
	);
	// The bulk endpoint (TASK-1668) gates on workspace owner/editor role —
	// it is NOT grant-aware like single-item CRUD. So the lane BULK actions
	// (move/tag/untag/set-priority/assign/archive-all) are gated on role
	// here, not canEditThisCollection, or a collection-edit-grant guest
	// would see them and get a 403 per click (TASK-1672 / Codex round 3).
	// The single `+` create stays on canEditThisCollection (grant-aware).
	let canBulkEdit = $derived(['owner', 'editor'].includes(workspaceStore.currentRole ?? ''));

	// Persist view mode to localStorage per collection
	function saveViewMode(mode: ViewMode) {
		viewMode = mode;
		if (collSlug) {
			try { localStorage.setItem(`pad-view-${collSlug}`, mode); } catch {}
		}
	}

	function loadSavedViewMode(coll: string, defaultMode: ViewMode): ViewMode {
		try {
			const saved = localStorage.getItem(`pad-view-${coll}`);
			if (saved === 'list' || saved === 'board' || saved === 'table') return saved;
		} catch {}
		return defaultMode;
	}

	// Persist the page-wide sort per collection (mirrors saveViewMode).
	function saveSortMode(mode: SortMode) {
		sortMode = mode;
		if (collSlug) {
			try { localStorage.setItem(`pad-sort-${collSlug}`, mode); } catch {}
		}
	}

	function loadSavedSortMode(coll: string): SortMode {
		try {
			const saved = localStorage.getItem(`pad-sort-${coll}`);
			if (SORT_OPTIONS.some((o) => o.value === saved)) return saved as SortMode;
		} catch {}
		return 'manual';
	}

	// Sort options available for this collection: hide "Priority" when the
	// collection has no `priority` select field (it would be a no-op).
	let sortOptions = $derived(
		collection && priorityField(collection)
			? SORT_OPTIONS
			: SORT_OPTIONS.filter((o) => o.value !== 'priority')
	);

	// Fall back to 'manual' if the active sort isn't valid for this
	// collection (e.g. a 'priority' choice persisted for a collection
	// that has no priority field after switching collections).
	$effect(() => {
		if (!sortOptions.some((o) => o.value === sortMode)) {
			sortMode = 'manual';
		}
	});

	// Sync filters to URL query params (shareable). The full param-build
	// (view/filters/tags/unparented/search, plus preserving an open split
	// pane's `?item=` across the rebuild — PLAN-2105) is factored into
	// `buildCollectionUrlParams` (TASK-2116) so it's unit-testable without
	// mounting this route; this function just supplies the live state and
	// URL and performs the actual navigation.
	function updateUrlFilters() {
		if (!collSlug || !wsSlug) return;
		const params = buildCollectionUrlParams(
			{ viewMode, activeFilters, selectedTags, unparentedApplied, searchQuery },
			page.url
		);
		const qs = params.toString();
		const newUrl = `/${username}/${wsSlug}/${collSlug}${qs ? '?' + qs : ''}`;
		goto(newUrl, { replaceState: true, noScroll: true, keepFocus: true });
	}

	// ── Split-pane open/close (PLAN-2105 Phase 2) ──────────────────────
	// Toggle the `?item=` query param that `openItemRef` derives from.
	// Both helpers preserve every OTHER query param (view/sort/filter/
	// tags/search) by mutating a clone of the live URL rather than
	// rebuilding from filter state, and reuse the same
	// `{ noScroll, keepFocus }` goto options as `updateUrlFilters`.
	//
	// History policy (deliberate split):
	//  • The INITIAL open PUSHES a history entry (replaceState:false) so a
	//    single Back closes the pane — matching "back/forward work
	//    naturally".
	//  • RE-TARGETING an already-open pane (row-click / Enter / j-k moving
	//    A→B→C) REPLACES, so paging through N rows doesn't stack N history
	//    entries that Back must unwind before it can close the pane
	//    (PLAN-2105 history policy; Codex round 2 P2). `closeItemPane`
	//    likewise REPLACES.
	function openItemPane(item: Item) {
		const url = new URL(page.url);
		// Whether a pane is ALREADY open decides push (first open) vs replace
		// (re-target). Read off the live URL — the same source openItemRef
		// derives from.
		const alreadyOpen = url.searchParams.has('item');
		url.searchParams.set('item', itemUrlId(item));
		goto(`${url.pathname}${url.search}`, {
			replaceState: alreadyOpen,
			noScroll: true,
			keepFocus: true,
		});
	}

	function closeItemPane() {
		const url = new URL(page.url);
		url.searchParams.delete('item');
		goto(`${url.pathname}${url.search}`, {
			replaceState: true,
			noScroll: true,
			keepFocus: true,
		});
	}

	// ── Resizable detail pane + persisted width (PLAN-2105 / TASK-2114) ─
	// No resize primitive existed in the repo, so this is built from
	// scratch: a draggable divider between the list column and the pane,
	// pointer-captured so the drag can't select text or fall through to a
	// row-click. The width persists to localStorage under a GLOBAL key that
	// deliberately does NOT collide with the vestigial `pad-detail-panel`
	// key in ui.svelte.ts, reusing the read-on-init / write-on-change idiom
	// from that store (the `pad-topbar` pattern).
	const PANE_WIDTH_KEY = 'pad-pane-width';
	const PANE_WIDTH_MIN = 360; // mirrors the CSS clamp() floor
	const PANE_WIDTH_MAX = 720; // generous ceiling (CSS clamp maxed at 640)
	const PANE_WIDTH_DEFAULT = 460;
	const PANE_DIVIDER_WIDTH = 6; // keep in sync with .pane-divider flex-basis
	// Keep at least this much room for the list column so a wide drag (or a
	// stored width carried over from a bigger screen) can never crush the
	// list to nothing.
	const LIST_MIN_WIDTH = 360;

	// Min/max pane width for the CURRENT container. Measures the ACTUAL
	// flex-row container (`.collection-page` — which spans `.main-content`,
	// i.e. EXCLUDES the sidebar + page chrome) once the pane is mounted, so
	// the clamp reflects real available space and a drag can never crush the
	// list. Before the pane mounts (init / keyboard) it falls back to the
	// viewport as a loose upper bound; the ResizeObserver below refits once
	// the real container is measurable (Codex round 1 P2).
	//
	// When the container is too narrow to honor BOTH the pane floor and the
	// list floor (e.g. a ~800px viewport with the sidebar expanded), the fixed
	// 360px floors would force the list down to ~140px. In that regime both
	// floors relax to 40% of the usable width so neither column is crushed —
	// the pane can shrink below PANE_WIDTH_MIN and the list keeps ≥40% (Codex
	// round 2 P2).
	function paneBounds(): { min: number; max: number } {
		let containerW = 0;
		const container = paneEl?.parentElement ?? null;
		if (container) containerW = container.getBoundingClientRect().width;
		else if (browser) containerW = window.innerWidth;
		if (containerW <= 0) return { min: PANE_WIDTH_MIN, max: PANE_WIDTH_MAX };
		const usable = containerW - PANE_DIVIDER_WIDTH;
		const listFloor = Math.min(LIST_MIN_WIDTH, usable * 0.4);
		const paneFloor = Math.min(PANE_WIDTH_MIN, usable * 0.4);
		const max = Math.max(paneFloor, Math.min(PANE_WIDTH_MAX, usable - listFloor));
		return { min: paneFloor, max };
	}
	function clampPaneWidth(px: number): number {
		const { min, max } = paneBounds();
		return Math.max(min, Math.min(max, px));
	}

	// The RAW persisted preference (unclamped) — the width the user last
	// chose. Kept separate from the APPLIED width so a temporarily narrow
	// container (small window / open sidebar) refits the pane down without
	// destroying the preference, and widening restores it.
	function readStoredPaneWidth(): number | null {
		if (!browser) return null;
		try {
			const raw = localStorage.getItem(PANE_WIDTH_KEY);
			if (!raw) return null;
			const n = Number(raw);
			return Number.isFinite(n) ? n : null;
		} catch {
			return null;
		}
	}

	// storedPaneWidth = the raw preference; paneWidth = that preference
	// clamped to what currently fits. BOTH read synchronously from
	// localStorage on the first client tick. These authenticated routes are
	// CSR-only (adapter-static `fallback` ⇒ no runtime SSR), so the
	// `!browser` → null branch only runs at build-time fallback generation,
	// never for a real user: the persisted width is present on the very first
	// client paint and a direct `?item=REF` landing shows no jump from the
	// CSS default to the stored width (PLAN-2105 reflow mitigation). Applied
	// as the `--pane-width` CSS var on the pane below; `null` → the CSS
	// clamp() default.
	let paneEl = $state<HTMLElement | null>(null);
	let resizingPane = $state(false);
	// Read once into a plain const so neither $state initializer references the
	// other reactive value (which would only capture its initial value anyway).
	const initialStoredWidth = readStoredPaneWidth();
	let storedPaneWidth = $state<number | null>(initialStoredWidth);
	let paneWidth = $state<number | null>(
		initialStoredWidth != null ? clampPaneWidth(initialStoredWidth) : null,
	);
	// The pane's real rendered width, tracked by the ResizeObserver below.
	// Fallback for `aria-valuenow` in the brief window before the observer
	// first fires and sets `paneWidth`.
	let measuredPaneWidth = $state<number | null>(null);
	// The current bounds, mirrored into state so the divider's ARIA range
	// tracks the relaxed floors on a cramped container (Codex round 3 P3).
	let ariaMin = $state(PANE_WIDTH_MIN);
	let ariaMax = $state(PANE_WIDTH_MAX);

	// Commit a new user-chosen width: clamp, apply, persist the preference.
	function setPaneWidth(px: number) {
		const clamped = clampPaneWidth(px);
		storedPaneWidth = clamped;
		paneWidth = clamped;
		if (browser) {
			try {
				localStorage.setItem(PANE_WIDTH_KEY, String(clamped));
			} catch {}
		}
	}

	// The width to APPLY for the current container: the user's saved
	// preference if set, otherwise the CSS `clamp(360px, 38%, 640px)` default
	// computed in JS — both clamped to what currently fits. Mirroring the CSS
	// default means applying it as `--pane-width` matches what CSS renders on
	// roomy layouts (so the observer's first run causes no reflow) while a
	// cramped container gets a fitted width that doesn't crush the list, even
	// with NO saved preference (Codex round 3 P2).
	function fittedPaneWidth(): number {
		const { min, max } = paneBounds();
		let containerW = 0;
		const container = paneEl?.parentElement ?? null;
		if (container) containerW = container.getBoundingClientRect().width;
		else if (browser) containerW = window.innerWidth;
		const cssDefault = Math.min(640, Math.max(360, containerW * 0.38));
		return Math.max(min, Math.min(max, storedPaneWidth ?? cssDefault));
	}

	// Re-fit the applied width + ARIA bounds to the current container. Called
	// from the ResizeObserver on mount, window resize, and sidebar toggle.
	function refitPaneWidth() {
		paneWidth = fittedPaneWidth();
		const b = paneBounds();
		ariaMin = Math.round(b.min);
		ariaMax = Math.round(b.max);
	}

	// Fit the pane to its real container: once synchronously before paint (so a
	// direct `?item=` load never flashes an over-wide width) and then on every
	// container resize via a ResizeObserver. Also samples the rendered width for
	// ARIA. Effect re-runs when the pane mounts (paneEl binds).
	$effect(() => {
		if (!browser || !openItemRef || !paneEl) return;
		const el = paneEl;
		const container = el.parentElement;
		if (!container) return;
		// Synchronous FIRST fit against the real container, BEFORE the browser
		// paints. This effect runs after the pane is in the DOM but within the
		// same update cycle (pre-paint), so on a direct `/{ws}/{coll}?item=REF`
		// load it corrects the init clamp — which used `window.innerWidth` and
		// therefore over-counted by the ~260px sidebar — against the true
		// flex-row width with NO visible jump. Waiting for the observer's first
		// (async, post-paint) callback is what let an over-wide stored width
		// flash before snapping (TASK-2114 hydration-reflow criterion; coord P2).
		// Runs regardless of ResizeObserver support (needs only
		// getBoundingClientRect). `untrack` so reading storedPaneWidth/paneEl
		// inside the fit doesn't add them as deps of this observer effect (which
		// would re-create the observer on every drag).
		untrack(() => {
			refitPaneWidth();
			measuredPaneWidth = el.getBoundingClientRect().width;
		});
		// Observe the flex-row container so pane width refits on window resize
		// AND sidebar toggles (which resize the container without a window
		// `resize` event). Resizing the pane itself doesn't change the container
		// width, so this can't loop.
		if (typeof ResizeObserver === 'undefined') return;
		const ro = new ResizeObserver(() => {
			refitPaneWidth();
			measuredPaneWidth = el.getBoundingClientRect().width;
		});
		ro.observe(container);
		return () => ro.disconnect();
	});

	// Safety net for an interrupted drag: if the pane closes (ESC / Back /
	// nav removes the divider) mid-drag, pointerup/lostpointercapture may
	// never fire, leaving text selection disabled and the col-resize cursor
	// stuck across the whole app. When `?item=` clears, reset the flag and
	// restore the global drag chrome the pointer handlers would have (Codex
	// round 2 P2).
	$effect(() => {
		if (openItemRef) return;
		if (resizingPane) {
			resizingPane = false;
			if (browser) {
				document.body.style.userSelect = '';
				document.body.style.cursor = '';
			}
		}
	});

	function onDividerPointerDown(e: PointerEvent) {
		// Left button / touch / pen only; ignore right-click etc.
		if (e.button !== 0) return;
		// Stop the browser from starting a text selection or handing the
		// gesture to the list (row-click nav) mid-drag.
		e.preventDefault();
		resizingPane = true;
		const divider = e.currentTarget as HTMLElement;
		divider.setPointerCapture(e.pointerId);
		document.body.style.userSelect = 'none';
		document.body.style.cursor = 'col-resize';
	}

	function onDividerPointerMove(e: PointerEvent) {
		if (!resizingPane || !paneEl) return;
		// The pane is right-docked, so its width is (its right edge − pointer
		// x). Reading the live rect keeps this correct regardless of page
		// padding or the pane's current size.
		const rect = paneEl.getBoundingClientRect();
		setPaneWidth(rect.right - e.clientX);
	}

	function endPaneResize(e: PointerEvent) {
		if (!resizingPane) return;
		resizingPane = false;
		const divider = e.currentTarget as HTMLElement;
		try {
			divider.releasePointerCapture(e.pointerId);
		} catch {}
		document.body.style.userSelect = '';
		document.body.style.cursor = '';
	}

	// The pane's actual current width — the applied width if set, else the
	// real rendered size (the CSS clamp() default), so the FIRST keyboard
	// nudge moves from where the pane visibly is rather than an assumed
	// default that could be on the wrong side of it (Codex round 1 P2).
	function currentPaneWidth(): number {
		if (paneWidth != null) return paneWidth;
		if (browser && paneEl) return paneEl.getBoundingClientRect().width;
		return PANE_WIDTH_DEFAULT;
	}

	// Keyboard resize for the focusable separator (arrows nudge the width).
	// ArrowLeft grows the pane (divider moves left), ArrowRight shrinks it,
	// matching the drag direction.
	function onDividerKeydown(e: KeyboardEvent) {
		const step = e.shiftKey ? 32 : 16;
		const current = currentPaneWidth();
		if (e.key === 'ArrowLeft') {
			e.preventDefault();
			setPaneWidth(current + step);
		} else if (e.key === 'ArrowRight') {
			e.preventDefault();
			setPaneWidth(current - step);
		}
	}

	// Read filters from URL on load
	function loadUrlFilters() {
		const url = new URL(page.url);
		const filters: Record<string, string> = {};
		// Reset tags (and the unparented intent) up-front so navigating to a
		// collection whose URL has no `tags`/`$unparented` param clears any
		// selection carried over from the previous collection (absent =
		// cleared). The unparented intent is read here optimistically — a
		// restricted caller's URL carrying it gets corrected by the
		// restricted-clearing effect once `unparentedMetadataAvailable`
		// resolves (DR-2); nothing in this function's synchronous read can
		// know that yet.
		selectedTags = [];
		const unparentedIntent = readUnparentedParam(url.searchParams);
		// `item` is the split-pane param (PLAN-2105) — whitelist it (via the
		// shared `KNOWN_COLLECTION_URL_PARAMS`, TASK-2116) so it's not
		// misread as a schema-field filter and absorbed into activeFilters.
		// It's consumed by the `openItemRef` derived, not by the filter load
		// cycle.
		const knownParams = new Set([...KNOWN_COLLECTION_URL_PARAMS, UNPARENTED_FILTER_FIELD]);
		for (const [k, v] of url.searchParams.entries()) {
			if (k === 'view' && (v === 'list' || v === 'board')) {
				viewMode = v;
			} else if (k === 'q') {
				searchQuery = v;
			} else if (k === 'tags') {
				selectedTags = v.split(',').map((t) => t.trim()).filter(Boolean);
			} else if (!knownParams.has(k)) {
				filters[k] = v;
			}
		}
		// Mutual exclusivity (PLAN-2095 DR-3): a hand-crafted or legacy URL
		// could carry both a specific-parent filter (`parent`/`phase`) AND
		// the unparented pseudo-param at once. Resolve it the same way the
		// interactive chip toggle does — unparented wins (Codex review
		// round 1).
		const resolved = resolveParentUnparentedMutex(filters, unparentedIntent);
		unparentedFilter = resolved.unparented;
		// Assign `activeFilters` whenever the URL expressed EITHER kind of
		// filter intent — plain field params OR the unparented pseudo-param
		// on its own. Gating on `filters` alone (Codex review round 2, P2)
		// missed the "URL carries only `$unparented=true`, no field params"
		// case: `filters` is empty there, so the old guard left a STALE
		// `activeFilters.parent` from a previous navigation in place
		// alongside the freshly-applied unparented intent — breaking the
		// mutex and producing a silently-empty result instead of the
		// mutex-resolved `{}` this URL actually describes.
		if (Object.keys(filters).length > 0 || unparentedIntent) {
			activeFilters = resolved.filters;
		}
	}

	$effect(() => {
		if (wsSlug && collSlug) loadCollection(wsSlug, collSlug, showArchived);
	});

	// ── Scroll position persistence (TASK-755 → BUG-1425) ──────────────
	// Wire SvelteKit's snapshot API through the shared scroll-restoration
	// helper. The helper:
	//   1. Captures scrollY into sessionStorage on navigate-away (per
	//      history entry — back/forward each get the right offset).
	//   2. Restores via double-RAF gated on `loading` so the document has
	//      hydrated enough that scrollTo isn't clamped to ~0.
	//   3. Mirrors to localStorage under `persistKey` for cross-tab /
	//      tab-close-reopen restoration (the workspace-switcher TASK-754
	//      route restore path).
	//
	// `persistKey` includes pathname, search, and `showArchived` so each
	// filter / view-mode URL gets its own localStorage entry — toggling
	// archived (which changes the dataset but not the URL) doesn't apply
	// a stale offset across the two views.
	//
	// Scope is intentionally page-level scroll only. Board view's internal
	// horizontal scroll container is not captured here.
	const scrollRestoration = createScrollRestoration({
		// `collection?.slug === collSlug` is the identity check that
		// prevents firing against stale content when the same component
		// instance handles a different collection URL (e.g. /tasks →
		// /bugs). `length > 0` deliberately omitted (Codex P2 round 2).
		ready: () => !loading && collection?.slug === collSlug,
		// persistKey is intentionally pathname-only — filter / view /
		// archive toggles call `goto({ replaceState: true })` which
		// rewrites `page.url.search`, and if those were in the key the
		// helper would treat each filter combo as a separate entry,
		// clear `pending`, re-read LS, and restore-jump the user mid-
		// interaction. Codex BUG-1425 round 5 P2-A flagged this as a
		// regression vs. TASK-755's pathname-only restore gate. We
		// trade per-filter offset granularity (was per-`?status=open`)
		// for stable in-page filter behavior.
		persistKey: () =>
			wsSlug ? `pad-last-scroll-${wsSlug}-${page.url.pathname}` : null,
	});
	export const snapshot = scrollRestoration.snapshot;

	// Reflect the collection name in the browser tab; clear any stale item ref.
	//
	// Title precedence (PLAN-2105 / TASK-2112): when a detail pane is open the
	// paned item's title WINS the tab title, so yield here while `?item=` is
	// set. The embedded ItemDetail owns the title whenever it's mounted; this
	// effect depends on `openItemRef`, so it re-runs and reclaims the tab
	// title the instant the pane closes.
	$effect(() => {
		if (openItemRef) return;
		titleStore.setPageTitle({
			section: collection?.name ?? null,
			item: null,
		});
	});

	// Subscribe to SSE events for live updates to this collection's items
	let unsubscribeSSE: (() => void) | null = null;

	$effect(() => {
		// Clean up previous subscription
		unsubscribeSSE?.();
		unsubscribeSSE = null;

		if (!wsSlug || !collSlug) return;

		const ws = wsSlug;
		const coll = collSlug;

		unsubscribeSSE = sseService.onItemEvent(async (event) => {
			// React to item lifecycle events by pulling deltas through
			// the local store. With seq-stamped events (TASK-1358) we
			// can short-circuit duplicates the server's replay buffer
			// re-delivers after a tab-resume, or events whose data is
			// already in the local index from a prior delta. Anything
			// not "stale" still needs row data, which only
			// `deltaSync` can fetch — the SSE wire payload doesn't
			// carry it. We don't filter by collection here: an item
			// moved into or out of this collection still needs its
			// delta applied so the derived view reflects it.
			const status = localIndex.classifySSEEvent(ws, event);
			if (status === 'stale') return;
			await deltaSync(ws);
		});
	});

	// Sync coordinator — handle tab-resume data refresh efficiently
	let unsubscribeSync: (() => void) | null = null;

	onMount(() => {
		unsubscribeSync = syncService.onSync(async (result) => {
			if (!wsSlug || !collSlug) return;

			// Always run deltaSync, even for `caught_up` — SSE
			// delivers events, not delta data, and a previous
			// deltaSync failure (Codex P2 round 2) won't recover
			// without a fresh fetch attempt. The localIndex cursor is
			// independent of syncService.lastSyncTime, and per-row
			// seq guards make repeated calls idempotent.
			const ok = await deltaSync(wsSlug);
			await refreshProgress(wsSlug, collSlug, items);
			if (ok && result.type === 'full_refresh') {
				// Only advance the legacy syncService cursor on a
				// clean catch-up. A failed reconcile leaves it where
				// it is so the next tab-resume retries.
				syncService.markSynced();
			}
		});
	});

	onDestroy(() => {
		unsubscribeSSE?.();
		unsubscribeSync?.();
		// Scroll save/restore cleanup is owned by createScrollRestoration
		// (snapshot.capture fires on navigate-away; the helper's $effect
		// teardown cancels any in-flight RAF).
	});

	/**
	 * Drive a /items-changes delta apply against the local store. Used
	 * by the SSE event handler and the syncService incremental path —
	 * both signal "something on the server changed; reconcile". The
	 * store's per-row seq guards make the call idempotent even when
	 * SSE + sync both fire for the same change.
	 *
	 * Loops until the cursor stops advancing (the server caps each
	 * response; deeply-behind tabs need multiple pages). Hard cap at
	 * 50 iterations to defend against pathological loops, matching
	 * localIndex.bootstrap's reconcile loop.
	 *
	 * Returns true on a clean catch-up, false on any failure / cap
	 * trip so the caller can avoid advancing its own cursor against a
	 * stale local cache (Codex P2 round 1 of TASK-1357).
	 */
	async function deltaSync(ws: string): Promise<boolean> {
		try {
			for (let i = 0; i < 50; i++) {
				const epochBefore = localIndex.scopeEpochFor(ws);
				const since = localIndex.cursorFor(ws);
				const delta = await api.items.changes(ws, since);
				if (localIndex.scopeEpochFor(ws) !== epochBefore) {
					// A concurrent resync installed a new snapshot + pinned
					// cursor while this request was in flight; the response
					// predates it. Re-poll from the new cursor rather than
					// reporting catch-up on stale data (Codex P2 round 8).
					continue;
				}
				if (await localIndex.ensureProjectionScope(ws, delta.includes_unparented_metadata)) {
					// A resync just pinned the cursor to the snapshot cursor to
					// replay post-snapshot mutations under the new scope. Keep
					// looping so the next `/items-changes` actually fetches them
					// instead of reporting caught-up prematurely. The resync
					// already aligned the scope, so ensureProjectionScope won't
					// re-fire; the 50-iteration cap bounds the loop.
					continue;
				}
				if (delta.changes.length === 0 || delta.cursor === since) {
					deltaSyncFailed = false;
					// This loop is an independent reconcile path from
					// `bootstrap()`'s internal one (driven by SSE/periodic
					// sync, not the initial cold/warm boot) — it's the only
					// owner of `pendingResync` for a resync IT triggered.
					// Mark caught up so a mid-session projection-scope
					// change doesn't leave `pendingResyncFor` stuck `true`
					// for the rest of the session (TASK-2099 / PLAN-2095
					// DR-2, Codex review round 4). Pass `epochBefore` — this
					// iteration already confirmed it still matches the live
					// epoch above — so a differently-scoped resync that
					// lands concurrently after this point isn't silently
					// stomped (Codex review round 5).
					localIndex.markCaughtUp(ws, epochBefore);
					return true;
				}
				localIndex.applyDelta(
					ws,
					delta.changes,
					delta.cursor,
					delta.includes_unparented_metadata,
				);
				if (delta.cursor === since) {
					deltaSyncFailed = false;
					localIndex.markCaughtUp(ws, epochBefore);
					return true;
				}
			}
			// Cap hit — pretend success at the page level so we don't
			// thrash, but tell the caller it wasn't a clean catch-up.
			return false;
		} catch (err) {
			// 401 (session expired) / 403 (access revoked) mean the
			// cache is no longer ours to display. Drop it through the
			// same path bootstrap uses so the 403 handler (TASK-1360)
			// + 401 /login redirect (already in api/client.ts) can
			// react. Set `deltaSyncFailed` so the page surfaces an
			// error banner — `localIndex.reset` rolls state back to
			// 'cold' but the bootstrap effect can't re-trigger on the
			// same wsSlug/userId, so without this flag the page
			// pins at "Loading…" forever (Codex P2 round 5).
			if (
				err instanceof PadApiError &&
				(err.code === 'forbidden' || err.code === 'unauthorized')
			) {
				localIndex.reset(ws);
				deltaSyncFailed = true;
			}
			return false;
		}
	}

	async function refreshProgress(ws: string, coll: string, itemList: typeof items) {
		if (coll === 'plans') {
			const progress = await api.items.plansProgress(ws).catch(() => []);
			itemProgress = plansProgressToMap(progress);
			progressLabel = 'tasks';
		} else {
			// Non-plans collections: prefer child-item progress (real linked
			// children) per item; fall back to markdown-checkbox progress for
			// items with none (BUG-1509). `showArchived` keeps badges on
			// archived items (PR #491 [P2]). Shared fetch+merge (TASK-2029).
			itemProgress = await fetchCollectionProgress(ws, coll, { includeArchived: showArchived });
		}
	}

	// Monotonic load token (plain, non-reactive) guarding against a
	// superseded fetch committing stale state. A slow load for
	// collection A that resolves AFTER the route already switched to B
	// must not overwrite B's `collection` / `metaError` / `metaLoading`
	// (Codex race finding). Every entry bumps the token; each awaited
	// result checks it's still the latest before writing.
	let loadSeq = 0;

	async function loadCollection(ws: string, coll: string, includeArchived = false) {
		const seq = ++loadSeq;
		metaLoading = true;
		metaError = null;
		// Reset for the new route; only flips back to `true` once
		// `loadUrlFilters()` below has actually run for THIS load (guarded
		// by the same `seq !== loadSeq` supersede-check). Deliberately NOT
		// derived from `metaLoading` alone (Codex review round 6): on a
		// failed load (a genuine 404, or a transient error), `metaLoading`
		// still flips back to `false` in the `finally` block below even
		// though `loadUrlFilters()` never ran — `urlFiltersLoaded` stays
		// `false` through that path, correctly keeping the unparented
		// URL-sync effect from firing with stale filter state while this
		// route is erroring.
		urlFiltersLoaded = false;
		try {
			// Items now flow through localIndex (the `items` $derived
			// above reads `getByCollection`). We still fetch the
			// collection metadata + saved views + members directly,
			// AND ensure the workspace is bootstrapped — but the
			// bootstrap effect upstream is what drives the items
			// store, so the parallel work is metadata-only here.
			const [collData, viewsData, membersData] = await Promise.all([
				api.collections.get(ws, coll),
				api.views.list(ws, coll).catch(() => [] as View[]),
				api.members.list(ws).catch(() => ({ members: [], invitations: [] })),
			]);
			// A newer load started while this one was in flight — drop
			// this result so it can't overwrite the current route's state.
			if (seq !== loadSeq) return;
			collection = collData;
			savedViews = viewsData;
			workspaceMembers = membersData.members ?? [];
			activeViewId = null;

			// Fetch progress badges for the collection's items. Shared
			// fetch+merge helpers (TASK-2029); the seq-guard, label, and
			// error handling stay here (per call site).
			if (coll === 'plans') {
				try {
					const progress = await api.items.plansProgress(ws);
					if (seq !== loadSeq) return;
					itemProgress = plansProgressToMap(progress);
					progressLabel = 'tasks';
				} catch {
					// Don't clear a newer load's progress badges if this
					// stale load's progress fetch rejected (Codex round 5).
					if (seq !== loadSeq) return;
					itemProgress = {};
				}
			} else {
				// Non-plans collections: prefer child-item progress (real
				// linked children, label "tasks") per item; fall back to
				// markdown-checkbox progress (label "done") for items that
				// have no linked children (BUG-1509). `includeArchived`
				// keeps the archived-items toggle's badges (PR #491 [P2]).
				try {
					const map = await fetchCollectionProgress(ws, coll, { includeArchived });
					if (seq !== loadSeq) return;
					itemProgress = map;
					progressLabel = 'done';
				} catch {
					// Don't clear a newer load's progress badges if this
					// stale load's progress fetch rejected (Codex round 5).
					if (seq !== loadSeq) return;
					itemProgress = {};
				}
			}

			// `relationLabels` is computed reactively below — no need
			// to populate it here. (Pre-localIndex this was a one-shot
			// fetch; now plans flow into the local store and the
			// derived map below picks them up as they hydrate.)

			// A newer load may have started during the progress awaits —
			// don't apply this (stale) collection's view/sort/filter state
			// over the current route's (Codex round 4).
			if (seq !== loadSeq) return;
			// Set view mode: URL param > localStorage > collection default
			const settings = parseSettings(collData);
			const defaultMode = (['board', 'list', 'table'].includes(settings.default_view))
				? settings.default_view as ViewMode : 'list';
			viewMode = loadSavedViewMode(coll, defaultMode);
			sortMode = loadSavedSortMode(coll);

			// Override with URL params if present
			loadUrlFilters();
			urlFiltersLoaded = true;
		} catch (err) {
			// Superseded by a newer load — don't clobber the current
			// route's state with this stale failure.
			if (seq !== loadSeq) return;
			// Distinguish a genuine not-found from a transient failure
			// (BUG-2025). Only a real 404 (`not_found`) collapses to the
			// terminal "Collection not found" empty state; a network
			// blip / 5xx / 429 sets `metaError` so the template shows an
			// error + Retry affordance instead of masking a live
			// collection as deleted.
			if (err instanceof PadApiError && err.code === 'not_found') {
				collection = null;
				metaError = null;
				// `items` is derived from localIndex; nothing to clear here.
				// A missing collection shows the empty / not-found state via
				// the `collection` null branch in the template.
			} else {
				metaError = err instanceof Error ? err : new Error('Failed to load collection');
			}
		} finally {
			// Only the latest load owns the loading flag — a superseded
			// load must not flip it false out from under the current one.
			if (seq === loadSeq) metaLoading = false;
		}
	}

	let settings = $derived(collection ? parseSettings(collection) : null);
	let quickActions = $derived<QuickAction[]>(settings?.quick_actions ?? []);
	let schema = $derived(collection ? parseSchema(collection) : null);
	let groupField = $derived(
		viewMode === 'board'
			? (settings?.board_group_by ?? 'status')
			: (settings?.list_group_by ?? 'status')
	);

	let statusOptions = $derived(collection ? getStatusOptions(collection) : []);

	let filteredItems = $derived.by(() => {
		let result = items;

		// Apply field filters
		for (const [key, value] of Object.entries(activeFilters)) {
			result = result.filter((item) => {
				// Parent filter uses the parent link, not fields JSON
				// Also accept legacy 'phase' key for backward compat with saved views
				if (key === 'parent' || key === 'phase') {
					return item.parent_link_id === value;
				}
				const fields = parseFields(item);
				return fields[key] === value;
			});
		}

		// Apply tag filter (OR semantics): keep items carrying ANY selected
		// tag. Tags are a top-level column (not in `fields` JSON), so this
		// is its own branch — mirroring the `parent`/`phase` special-case
		// above. `parseTags` tolerates the JSON-array-string shape the
		// local index stores.
		if (selectedTags.length > 0) {
			const wanted = new Set(selectedTags);
			result = result.filter((item) =>
				parseTags(item).some((t) => wanted.has(t)),
			);
		}

		// Apply the "Unparented only" chip (TASK-2099 / PLAN-2095): keep
		// items the local-first projection marked structurally loose. Gated
		// on `unparentedApplied` (intent AND confirmed unrestricted scope —
		// DR-2), not raw `unparentedFilter`, so a restricted caller's
		// carried-over intent never filters anything.
		//
		// `!== false` (optimistic-inclusive), not `=== true`: a full
		// mutation response (create/update) intentionally omits local-
		// first-only projections (see `localIndex.svelte.ts::toSkinny` /
		// `preserveProjectionMetadata`), so a just-created item's optimistic
		// row carries `is_unparented: undefined` until the authoritative
		// index/delta row lands and merges it in. Requiring strict `true`
		// made a brand-new loose item — the exact case a user creating
		// while this filter is active most wants to see — flash out of the
		// list immediately after creation (Codex review round 2). Every
		// AUTHORITATIVE row for an unrestricted caller always carries the
		// bit (Phase 1 / TASK-2096), so `undefined` here is bounded to that
		// narrow optimistic window, not a permanent unknown; an item that
		// turns out to be parented is corrected out on the next delta.
		if (unparentedApplied) {
			result = result.filter((item) => item.is_unparented !== false);
		}

		// Apply search query. PLAN-1343 Phase 3b: the local path
		// populates `searchResultRank` synchronously (sub-ms) so the
		// filter just intersects. The substring fallback below covers
		// the body:/content: server-FTS path while its 200ms debounce
		// is in flight — and the very narrow window after a cold
		// workspace load where indexReady is still false.
		//
		// Critically: AFTER filtering, sort matched items by rank so
		// localSearch's exact-ref hoist + boost tuning (TASK-1367)
		// surfaces in the UI. Otherwise the natural `updated_at DESC`
		// order from `localIndex.getByCollection` would override the
		// ranking. Codex round 2 of TASK-1367.
		if (searchQuery.trim() && searchResultRank !== null) {
			const rank = searchResultRank;
			result = result
				.filter((item) => rank.has(item.id))
				.sort((a, b) => (rank.get(a.id) ?? 0) - (rank.get(b.id) ?? 0));
		} else if (searchQuery.trim()) {
			// Fallback to client-side substring scan while the server
			// FTS response is pending or the local index is mid-bootstrap.
			const q = searchQuery.trim().toLowerCase();
			result = result.filter((item) => {
				if (item.title.toLowerCase().includes(q)) return true;
				const fields = parseFields(item);
				return Object.values(fields).some(
					(v) => typeof v === 'string' && v.toLowerCase().includes(q)
				);
			});
		}

		return result;
	});

	let itemCounts = $derived.by(() => {
		if (!collection) return null;
		const statusField = schema?.fields.find((f) => f.key === 'status');
		if (!statusField?.options) return null;
		const counts: Record<string, number> = {};
		for (const opt of statusField.options) {
			counts[opt] = 0;
		}
		for (const item of items) {
			const fields = parseFields(item);
			const status = fields.status;
			if (status && counts[status] !== undefined) {
				counts[status]++;
			}
		}
		return counts;
	});

	// Per-collection tag counts for the filter chips. Computed client-side
	// from `items` (already collection-scoped via localIndex, and already
	// honoring the `showArchived` toggle) — no network round-trip, and the
	// counts reflect the full collection so each chip's number is stable
	// regardless of which other tags are selected (OR semantics). Ordered
	// by count desc then tag asc to match the workspace tags page.
	let tagCounts = $derived.by(() => {
		const counts = new Map<string, number>();
		for (const item of items) {
			for (const tag of parseTags(item)) {
				counts.set(tag, (counts.get(tag) ?? 0) + 1);
			}
		}
		return [...counts.entries()]
			.map(([tag, count]) => ({ tag, count }))
			.sort((a, b) => b.count - a.count || a.tag.localeCompare(b.tag));
	});

	// Tag suggestions for the lane "Tag all" action (TASK-1672) — the
	// workspace's tags by frequency.
	let tagSuggestions = $derived(tagCounts.map((t) => t.tag));

	const emptyHintMap: Record<string, string> = {
		tasks: '/pad break down my current work into tasks',
		ideas: "/pad I have an idea for...",
		plans: '/pad create a plan for what I\'m working on',
		docs: '/pad document the architecture of this project',
		conventions: '/pad what conventions should this project follow?',
		playbooks: '/pad set up playbooks for our workflow',
		bugs: '/pad triage open issues in this project',
	};

	let emptyHint = $derived(emptyHintMap[collSlug] ?? null);

	let filtersOpen = $state(false);
	let hasActiveFilters = $derived(searchQuery.trim() !== '' || Object.keys(activeFilters).length > 0 || selectedTags.length > 0 || unparentedApplied);

	// ── Viewport detection ───────────────────────────────────────────────
	// On mobile the 3-icon view toggle (list/board/table) is swapped for a
	// chip trigger that opens a BottomSheet with labeled options — the raw
	// icon glyphs are ambiguous on touch and a labeled sheet is clearer.
	// Desktop keeps the segmented toggle unchanged. Uses the shared breakpoint
	// store (TASK-2028).
	let viewSheetOpen = $state(false);

	// If the viewport crosses above the mobile breakpoint while the sheet is
	// open (e.g. rotation), close it so a return to mobile doesn't immediately
	// re-mount the open sheet. Reads the shared breakpoint flag; writes only
	// `viewSheetOpen`, so no self-invalidation.
	$effect(() => {
		if (!viewport.isMobile) viewSheetOpen = false;
	});
	let viewModeLabel = $derived(
		viewMode === 'list' ? 'List' : viewMode === 'board' ? 'Board' : 'Table'
	);

	function selectViewMode(mode: ViewMode) {
		saveViewMode(mode);
		updateUrlFilters();
		viewSheetOpen = false;
	}

	function singularName(): string {
		if (!collection) return 'item';
		const name = collection.name;
		// Simple singular: remove trailing 's' if present
		if (name.endsWith('s') && name.length > 1) {
			return name.slice(0, -1);
		}
		return name;
	}

	function handleFilterChange(filters: Record<string, string>) {
		activeFilters = filters;
		// Mutual exclusivity with the unparented chip (PLAN-2095 DR-3):
		// setting a specific-parent filter clears "Unparented only".
		if (filtersSetParent(filters) && unparentedFilter) {
			unparentedFilter = false;
		}
		updateUrlFilters();
	}

	// Mutual exclusivity in the other direction: switching the "Unparented
	// only" chip on clears any specific-parent filter (PLAN-2095 DR-3).
	function handleUnparentedChange(value: boolean) {
		unparentedFilter = value;
		if (value) {
			activeFilters = clearParentFilter(activeFilters);
		}
		updateUrlFilters();
	}

	function handleTagFilterChange(tags: string[]) {
		selectedTags = tags;
		updateUrlFilters();
	}

	function handleSearchChange(query: string) {
		// Pure setter — the reactive effect below runs the actual search.
		// Keeping this small means non-input entry points (URL load via
		// `loadUrlFilters`, programmatic clears) get the same search
		// behavior without duplicating the local/server dispatch logic
		// (Codex round 1 P2: shared `?q=body:foo` URLs previously
		// landed in the local fallback because `loadUrlFilters` set
		// `searchQuery` directly and never invoked the dispatch).
		searchQuery = query;
		updateUrlFilters();
	}

	// Search dispatch. Tracks `searchQuery` (any entry point that mutates
	// it — typed input, URL load, programmatic clears), `wsSlug` /
	// `collSlug` (navigation across workspaces or collections must drop
	// stale results and re-issue against the new route — Codex round 2
	// P2), `showArchived` (re-run local search when archived rows toggle
	// in/out of scope), and `indexReady` (cold load: query typed before
	// bootstrap finishes; kick the search once the index hydrates).
	// Body-prefix queries route to server FTS with a 200ms debounce;
	// everything else hits the in-memory MiniSearch index synchronously.
	$effect(() => {
		void showArchived;
		void indexReady;
		// Track the localSearch mutation epoch so SSE-driven upserts /
		// removes refresh `searchResultRank` while a query is active —
		// without this, a row created after the query was typed would
		// stay hidden (and a row edited out of relevance would stay
		// visible) until the user retyped. Codex round 3 P2 of TASK-1364.
		void localSearch.epoch(wsSlug);
		const trimmed = searchQuery.trim();
		// Snapshot the route at effect-run time so a navigation mid-flight
		// can't let an old response populate the new route.
		const snapshotWs = wsSlug;
		const snapshotColl = collSlug;

		clearTimeout(searchTimeout);
		if (!trimmed) {
			searchResultRank = null; // null = no search active
			return;
		}

		// Reuse the page-level parsed query so the prefix vocabulary
		// (`body:`, `coll:`, `is:archived`, `#5`, ref) — TASK-1367 —
		// drives this effect AND the `items` derived view's archived
		// inclusion uniformly.
		const parsed = parsedSearch;

		// `body:` / `content:` prefix — fall through to the server FTS
		// endpoint, which searches the rich-text body. The local index
		// does not hold `content` by design (DOC-1342 decision #4), so
		// this prefix is the only way to grep bodies. A 200ms debounce
		// keeps the network path hammer-resistant while typing.
		if (parsed.body) {
			const bodyQuery = parsed.text;
			if (!bodyQuery) {
				searchResultRank = null;
				return;
			}
			// Clear stale results immediately so the substring fallback
			// renders while the network response is pending.
			searchResultRank = null;
			const snapshotQuery = trimmed;
			searchTimeout = setTimeout(async () => {
				try {
					const resp = await api.search(bodyQuery, {
						workspace: snapshotWs,
						collection: snapshotColl,
						limit: 200,
					});
					// Stale-response guard: drop the result if the user
					// navigated away or changed the query while the
					// request was in flight.
					if (
						searchQuery.trim() !== snapshotQuery ||
						wsSlug !== snapshotWs ||
						collSlug !== snapshotColl
					) {
						return;
					}
					searchResultRank = new Map(
						resp.results.map((r, i) => [r.item.id, i]),
					);
				} catch {
					if (
						searchQuery.trim() === snapshotQuery &&
						wsSlug === snapshotWs &&
						collSlug === snapshotColl
					) {
						searchResultRank = null;
					}
				}
			}, 200);
			return;
		}

		// Local path (default). MiniSearch is sub-millisecond for 5,000
		// rows — runs synchronously on every dependency change. PLAN-1343
		// Phase 3b acceptance: keystroke → first results <50ms P95.
		// When the index isn't ready yet (very narrow cold-load window),
		// leave `searchResultRank = null` so the substring fallback in
		// `filteredItems` kicks in until hydrate completes.
		if (!indexReady) {
			searchResultRank = null;
			return;
		}
		// Pass the raw query to localSearch — it owns prefix parsing so
		// the `coll:` / `is:archived` / `#N` / ref vocabulary works
		// uniformly across consumers (collection page + CommandPalette,
		// TASK-1367).
		const hits = localSearch.search(snapshotWs, trimmed, {
			collection: snapshotColl,
			includeArchived: showArchived,
			limit: 200,
		});
		searchResultRank = new Map(hits.map((h, i) => [h.id, i]));
	});

	async function handleStatusChange(item: Item, newValue: string) {
		if (!wsSlug) return;
		const fields = parseFields(item);
		fields[groupField] = newValue;
		const fieldsPayload = JSON.stringify(fields);
		const ws = wsSlug;
		const parentRef = formatItemRef(item) ?? item.slug;

		// Record the scope epoch at each request's issue time (BUG-2098). The
		// force-retry below can fire seconds later, after a user confirmation —
		// long enough for a projection resync to bump the epoch — so it must
		// capture its OWN epoch, not reuse the first request's. Setting it inside
		// doUpdate, right before the network call, gives each attempt the epoch
		// that was live when it was issued; a stale one makes upsert() refuse it.
		let epoch = localIndex.scopeEpochFor(ws);
		const doUpdate = (force: boolean) => {
			epoch = localIndex.scopeEpochFor(ws);
			return api.items.update(ws, item.id, { fields: fieldsPayload, ...(force ? { force: true } : {}) });
		};

		try {
			const updated = await doUpdate(false);
			// Push the canonical post-update row into the local index;
			// the `items` derived view re-renders automatically.
			localIndex.upsert(ws, updated, epoch);
			toastStore.show(`Moved to ${formatLabel(newValue)}`, 'success');
		} catch (e) {
			// BUG-1538 / TASK-1539: the server's open-children guard
			// (IDEA-1494) returns a structured 409 when transitioning a
			// parent to a terminal status while it still has open
			// children. Branch on the error shape so a USER cancel of
			// the modal doesn't get logged as an "update failure" and
			// the toast/log noise matches user intent.
			if (isOpenChildrenError(e)) {
				let forced;
				try {
					forced = await confirmOpenChildrenOrThrow(e, parentRef, () => doUpdate(true));
				} catch (retryErr) {
					// The retry-with-force itself failed (network /
					// 500 / fresh validation error). Surface that
					// distinctly from the original guard.
					const msg = retryErr instanceof Error ? retryErr.message : 'Failed to update status';
					console.error('Forced status update failed:', retryErr);
					toastStore.show(msg, 'error');
					throw retryErr;
				}
				if (forced) {
					localIndex.upsert(ws, forced, epoch);
					toastStore.show(`Moved to ${formatLabel(newValue)}`, 'success');
					return;
				}
				// User cancelled the override. Quiet info toast — this
				// is an intentional no-op, not a failure. Re-throw so
				// the BoardView drag-handler can unwind its optimistic
				// reorder.
				toastStore.show('Status change cancelled', 'info');
				throw e;
			}
			// Any other failure mode — network, validation, 500, etc.
			console.error('Failed to update item:', e);
			toastStore.show('Failed to update status', 'error');
			throw e; // Re-throw so BoardView knows the move failed
		}
	}

	async function handleReorder(updates: { slug: string; sort_order: number }[]) {
		if (!wsSlug) return;
		// Only persist items whose sort_order actually changed
		const dirty: { id: string; sort_order: number }[] = [];
		for (const { slug, sort_order } of updates) {
			const item = items.find((i) => i.slug === slug || i.id === slug);
			if (item && item.sort_order !== sort_order) {
				// Optimistic local update: upsert into the local index
				// with the new sort_order BEFORE awaiting the API. The
				// caller (e.g. ListView) doesn't await onReorder, so it
				// resyncs its rendered groups from `items` immediately
				// after drag-end — without an optimistic write the rows
				// snap back to the old order until the network PATCH
				// returns. Clearing `seq` on the optimistic copy
				// bypasses the per-row seq guard so the real API
				// response (with a higher seq) wins on arrival
				// (Codex P2 round 3 of TASK-1357).
				localIndex.upsert(wsSlug, {
					...item,
					sort_order,
					seq: undefined,
				});
				dirty.push({ id: item.id, sort_order });
			}
		}
		if (dirty.length === 0) return;
		// Persist to API sequentially (SQLite can't handle concurrent
		// writes), and upsert each returned row so the local index
		// settles back to the canonical server seq.
		try {
			for (const { id, sort_order } of dirty) {
				// Per-iteration epoch: a resync mid-loop must only reject the
				// settle-upserts for PATCHes issued before it, not later ones
				// issued under the new scope (BUG-2098). Capturing outside the
				// loop would wrongly reject every post-resync iteration. The
				// optimistic pre-writes above are synchronous (no await gap) so
				// they need no guard.
				const epoch = localIndex.scopeEpochFor(wsSlug);
				const updated = await api.items.update(wsSlug, id, { sort_order });
				localIndex.upsert(wsSlug, updated, epoch);
			}
		} catch (e) {
			console.error('Failed to persist sort order:', e);
		}
	}

	async function handleGroupReorder(newOrder: string[]) {
		if (!wsSlug || !collSlug || !collection) return;
		const currentSchema = parseSchema(collection);
		const fieldIdx = currentSchema.fields.findIndex((f) => f.key === groupField);
		if (fieldIdx === -1) return;

		// Update the field's options to the new order
		currentSchema.fields[fieldIdx].options = newOrder;
		const newSchemaStr = JSON.stringify(currentSchema);

		try {
			const updated = await api.collections.update(wsSlug, collSlug, { schema: newSchemaStr });
			collection = updated;
		} catch {
			toastStore.show('Failed to save column order', 'error');
		}
	}

	let creatingNew = $state(false);
	let quickCreateTitle = $state('');
	let quickCreateOpen = $state(false);
	let quickCreateInput = $state<HTMLInputElement>();

	async function createNewItem() {
		if (!wsSlug || !collSlug || creatingNew) return;
		creatingNew = true;
		try {
			const schema = collection ? parseSchema(collection) : { fields: [] };
			const defaultFields: Record<string, any> = {};
			const statusField = schema.fields.find(f => f.key === 'status');
			if (statusField?.options?.length) {
				defaultFields.status = statusField.options[0];
			}
			const item = await api.items.create(wsSlug, collSlug, {
				title: 'Untitled',
				content: '',
				fields: JSON.stringify(defaultFields),
				source: 'web'
			});
			goto(`/${username}/${wsSlug}/${collSlug}/${itemUrlId(item)}?new=1`);
		} catch (err: any) {
			if (isPlanLimitError(err)) {
				toastStore.show(planLimitMessage(err) + ' Upgrade to Pro', 'error', 6000, '/console/billing');
			} else {
				toastStore.show(err?.message || 'Failed to create item', 'error');
			}
		} finally {
			creatingNew = false;
		}
	}

	// Create an item in a board lane from the inline draft card
	// (TASK-1676, refines TASK-1671 / IDEA-1159). Pre-fills the lane's
	// group field so the item lands in that lane. `navigate` true (Enter
	// in the draft) opens the new item; false (nav-guard "Save") just
	// upserts it into the local index and stays put. Throws on failure so
	// BoardView can restore the draft. Returns the created item.
	async function quickCreateInColumn(
		groupValue: string,
		title: string,
		navigate: boolean
	): Promise<Item | null> {
		if (!wsSlug || !collSlug) return null;
		const trimmed = title.trim();
		if (!trimmed) return null;
		try {
			const schema = collection ? parseSchema(collection) : { fields: [] };
			const defaultFields: Record<string, any> = {};
			const statusField = schema.fields.find((f) => f.key === 'status');
			if (statusField?.options?.length) {
				defaultFields.status = statusField.options[0];
			}
			// Pre-fill the lane's group field (status, or a custom
			// board_group_by select) so the item opens in this lane.
			defaultFields[groupField] = groupValue;
			// Epoch before create: a brand-new id is never in the fence set,
			// so this is the case the epoch guard exists for (BUG-2098).
			const epoch = localIndex.scopeEpochFor(wsSlug);
			const item = await api.items.create(wsSlug, collSlug, {
				title: trimmed,
				content: '',
				fields: JSON.stringify(defaultFields),
				source: 'web'
			});
			if (navigate) {
				goto(`/${username}/${wsSlug}/${collSlug}/${itemUrlId(item)}?new=1`);
			} else {
				localIndex.upsert(wsSlug, item, epoch);
			}
			return item;
		} catch (err: any) {
			if (isPlanLimitError(err)) {
				toastStore.show(planLimitMessage(err) + ' Upgrade to Pro', 'error', 6000, '/console/billing');
			} else {
				toastStore.show(err?.message || 'Failed to create item', 'error');
			}
			throw err;
		}
	}

	// ── Inline draft cards: page-owned state + leave guard (TASK-1676) ──
	// State lives here (not in BoardView) so a draft survives a
	// board↔list view switch that unmounts BoardView, and so the leave
	// guard + dialog stay mounted regardless of view. BoardView binds
	// these and renders the per-lane draft cards.
	let draftText = $state<Record<string, string>>({});
	let draftOpen = $state<Record<string, boolean>>({});
	let savingDrafts = $state(false);
	let showLeaveDialog = $state(false);
	let pendingNav = $state<(() => void) | null>(null);
	let bypassNavGuard = false;

	let hasUnsavedDrafts = $derived(Object.values(draftText).some((t) => t.trim().length > 0));

	// Intercept in-app navigation while a draft is unsaved. `nav.to` is
	// null for full unload (reload / tab close / external) — drafts are
	// kept only until reload, so those aren't guarded.
	beforeNavigate((nav) => {
		if (bypassNavGuard || !hasUnsavedDrafts || !nav.to) return;
		// Only guard CLIENT-SIDE, in-app leaves we can replay with goto.
		// `willUnload` (external links, hard document navigations) and
		// full unload (reload / tab close, nav.to === null) are treated
		// like reload — drafts live only until then, and goto can't replay
		// a native navigation anyway (Codex round 3).
		if (nav.willUnload) return;
		// Same-pathname navigations are internal query/view state syncs
		// (updateUrlFilters' replaceState on a view/filter/search change),
		// not a real "leave" — the draft survives those (page state), so
		// prompting would be wrong (Codex round 2).
		if (nav.to.url.pathname === nav.from?.url.pathname) return;
		const url = nav.to.url;
		// Replay Back/Forward via history.go(delta) so cancelling +
		// re-navigating preserves history order (a goto would push the
		// target as a NEW entry, corrupting Back). Codex round 4.
		const popDelta = nav.type === 'popstate' ? nav.delta : undefined;
		nav.cancel();
		pendingNav = () => {
			bypassNavGuard = true;
			if (typeof popDelta === 'number') {
				history.go(popDelta);
				// popstate has no completion promise — reset the bypass
				// once the navigation has settled.
				setTimeout(() => { bypassNavGuard = false; }, 0);
			} else {
				goto(url).finally(() => { bypassNavGuard = false; });
			}
		};
		showLeaveDialog = true;
	});

	function runPendingNav() {
		const act = pendingNav;
		pendingNav = null;
		showLeaveDialog = false;
		act?.();
	}

	async function leaveSaveAll() {
		if (savingDrafts) return;
		savingDrafts = true;
		try {
			// Clear EACH draft as its create succeeds (not all at the end),
			// so retrying after a partial failure can't re-create the ones
			// that already saved (Codex round 1).
			for (const [col, text] of Object.entries(draftText)) {
				const title = text.trim();
				if (!title) continue;
				await quickCreateInColumn(col, title, false);
				delete draftText[col];
				delete draftOpen[col];
			}
		} catch {
			// A create failed (already toasted) — keep the dialog open with
			// the still-unsaved drafts so the user can retry or discard.
			savingDrafts = false;
			return;
		}
		savingDrafts = false;
		runPendingNav();
	}

	function leaveDiscard() {
		draftText = {};
		draftOpen = {};
		runPendingNav();
	}

	function leaveStay() {
		pendingNav = null;
		showLeaveDialog = false;
	}

	async function quickCreate() {
		const title = quickCreateTitle.trim();
		if (!title || !wsSlug || !collSlug || creatingNew) return;
		creatingNew = true;
		try {
			const schema = collection ? parseSchema(collection) : { fields: [] };
			const defaultFields: Record<string, any> = {};
			const statusField = schema.fields.find(f => f.key === 'status');
			if (statusField?.options?.length) {
				defaultFields.status = statusField.options[0];
			}
			// Epoch before create: brand-new id, never fenced (BUG-2098).
			const epoch = localIndex.scopeEpochFor(wsSlug);
			const item = await api.items.create(wsSlug, collSlug, {
				title,
				content: '',
				fields: JSON.stringify(defaultFields),
				source: 'web'
			});
			localIndex.upsert(wsSlug, item, epoch);
			quickCreateTitle = '';
			toastStore.show(`Created "${title}"`, 'success');
		} catch (err: any) {
			if (isPlanLimitError(err)) {
				toastStore.show(planLimitMessage(err) + ' Upgrade to Pro', 'error', 6000, '/console/billing');
			} else {
				toastStore.show(err?.message || 'Failed to create item', 'error');
			}
		} finally {
			creatingNew = false;
		}
	}

	function openQuickCreate() {
		quickCreateOpen = true;
		requestAnimationFrame(() => quickCreateInput?.focus());
	}

	function handleNewButtonClick() {
		if (quickCreateTitle.trim()) {
			quickCreate();
			return;
		}
		openQuickCreate();
	}

	function handleQuickCreateKeydown(e: KeyboardEvent) {
		if (e.key === 'Enter' && quickCreateTitle.trim()) {
			e.preventDefault();
			quickCreate();
		} else if (e.key === 'Escape') {
			quickCreateOpen = false;
			quickCreateTitle = '';
		}
	}

	// --- Keyboard navigation ---
	let focusedIndex = $state(-1);
	let focusedItemId = $derived(
		focusedIndex >= 0 && focusedIndex < filteredItems.length
			? filteredItems[focusedIndex].id
			: null
	);

	// Reset focus when items or filters change
	$effect(() => {
		filteredItems;
		focusedIndex = -1;
	});

	// Keep the row highlight on the OPEN pane's item (PLAN-2105 / TASK-2112).
	// `focusedItemId` (the marker List/Board/Table read) is otherwise driven
	// only by the keyboard cursor, so a click- / back-forward- / refresh-
	// opened pane wouldn't highlight its row. Snap the cursor to the paned
	// item whenever `?item=` (or the hydrated list) changes. Defined AFTER the
	// reset effect above so, when both fire on a filteredItems change, this one
	// wins and restores the paned row. j/k between openItemRef changes still
	// moves the cursor freely (this effect only re-runs on openItemRef /
	// filteredItems changes, not on focusedIndex).
	$effect(() => {
		if (!openItemRef) return;
		const idx = filteredItems.findIndex(
			(i) => itemUrlId(i) === openItemRef || i.slug === openItemRef,
		);
		if (idx >= 0) focusedIndex = idx;
	});

	// Register a Cmd+F handler with the layout while this page is mounted.
	// The layout only intercepts Cmd+F when a handler is registered, so on
	// pages without one (e.g. item view) it falls through to browser-native
	// find. (BUG-986)
	$effect(() => {
		uiStore.registerCollectionSearch(() => {
			if (!filtersOpen) {
				filtersOpen = true;
			}
			requestAnimationFrame(() => searchInputEl?.focus());
		});
		return () => uiStore.unregisterCollectionSearch();
	});

	function handlePageKeydown(e: KeyboardEvent) {
		// Don't capture when typing in inputs/textareas or when quick-create is open
		const tag = (e.target as HTMLElement)?.tagName;
		if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;
		// Also bail when typing inside a contenteditable (the Tiptap editor is
		// a contenteditable DIV) or anywhere inside the detail pane — otherwise
		// j/k/arrows/Enter/Escape typed in the open pane's editor would drive
		// list navigation instead of editing (PLAN-2105 / TASK-2111).
		if ((e.target as HTMLElement)?.closest?.('[contenteditable="true"], .item-pane')) return;
		if (quickCreateOpen || saveViewOpen) return;

		switch (e.key) {
			case 'j':
			case 'ArrowDown':
				e.preventDefault();
				if (filteredItems.length > 0) {
					focusedIndex = Math.min(focusedIndex + 1, filteredItems.length - 1);
					scrollFocusedIntoView();
				}
				break;
			case 'k':
			case 'ArrowUp':
				e.preventDefault();
				if (filteredItems.length > 0) {
					focusedIndex = Math.max(focusedIndex - 1, 0);
					scrollFocusedIntoView();
				}
				break;
			case 'Enter':
				if (focusedIndex >= 0 && focusedIndex < filteredItems.length) {
					e.preventDefault();
					const item = filteredItems[focusedIndex];
					// Enter opens the focused row in the split pane (PLAN-2105 /
					// TASK-2111) rather than navigating full-page.
					openItemPane(item);
				}
				break;
			case 'Escape':
				focusedIndex = -1;
				break;
		}
	}

	function scrollFocusedIntoView() {
		requestAnimationFrame(() => {
			const el = document.querySelector('.item-card.focused');
			if (el) {
				el.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
			}
		});
	}

	// The server's bulk endpoint caps a single request at 1000 ids
	// (maxBulkItems). The old per-item archive loop had no ceiling, so
	// chunk to keep very large filtered lanes working — one bulk call per
	// chunk (a few SSE events instead of thousands; still far better than
	// per-item). TASK-1672 / Codex round 1.
	const BULK_CHUNK = 1000;

	// Longer dwell for toasts carrying an Undo so the user can react
	// (TASK-1674).
	const UNDO_TOAST_MS = 7000;

	// Shared runner for the lane bulk actions (TASK-1672). Chunks ids,
	// deltaSyncs so the view reflects the change, and toasts the aggregate
	// updated/failed counts. `verb` is the past-tense toast word
	// ("Archived", "Moved", …). `opts.undo` (TASK-1674) adds an Undo button
	// to the success toast, invoked with the affected ids. Returns the
	// affected item ids on success; [] when nothing succeeded.
	// `ws` is captured by the caller at action time, NOT read live —
	// the undo toast lives 7s and is global, so navigating to another
	// workspace before clicking Undo must still target the workspace the
	// original action ran on (Codex round 1 of TASK-1674).
	async function runBulkOn(
		ws: string,
		req: BulkItemsRequest,
		verb: string,
		opts?: { undo?: (okIds: string[]) => void }
	): Promise<string[]> {
		if (!ws || req.ids.length === 0) return [];
		const okIds: string[] = [];
		let errMsg = '';
		// A thrown chunk (network / auth / 4xx) stops further chunks but
		// must NOT skip finalization: earlier chunks may already have
		// mutated up to 1000+ items server-side, so we still deltaSync and
		// report the partial result rather than leaving the UI stale behind
		// a generic error. Codex round 2.
		for (let i = 0; i < req.ids.length; i += BULK_CHUNK) {
			const chunk = req.ids.slice(i, i + BULK_CHUNK);
			try {
				const res = await api.items.bulk(ws, { ...req, ids: chunk });
				for (const u of res.updated) okIds.push(u.id);
			} catch (e: any) {
				errMsg = e?.message || '';
				break;
			}
		}
		const ok = okIds.length;
		// Everything not in okIds — per-row rejections AND items in an
		// un-attempted chunk after a throw — is a failure for the toast.
		const failed = req.ids.length - ok;
		// Sync so the succeeded rows update/disappear. On deltaSync failure
		// the mutation is still real server-side (SSE catches the cache up)
		// — soften the toast (matches the old archive flow, Codex P3 round 2
		// of TASK-1357).
		const synced = ok > 0 ? await deltaSync(ws) : true;
		// Attach an Undo button only when the action succeeded and the
		// caller supplied an undo (archive / move — the destructive ops).
		const undoAction =
			ok > 0 && opts?.undo
				? { label: 'Undo', onAction: () => opts.undo?.(okIds) }
				: undefined;
		const dwell = undoAction ? UNDO_TOAST_MS : undefined;
		if (ok === 0) {
			toastStore.show(errMsg || `Failed to ${verb.toLowerCase()} items`, 'error');
		} else if (failed > 0) {
			toastStore.show(
				`${verb} ${ok} item${ok !== 1 ? 's' : ''}, ${failed} failed`,
				'success',
				dwell,
				undefined,
				undoAction
			);
		} else {
			toastStore.show(
				`${verb} ${ok} item${ok !== 1 ? 's' : ''}${synced ? '' : ' (updating…)'}`,
				'success',
				dwell,
				undefined,
				undoAction
			);
		}
		return okIds;
	}

	// Convenience for the non-deferred actions: bind to the live workspace
	// at call time (these complete immediately, no cross-workspace race).
	function runBulk(
		req: BulkItemsRequest,
		verb: string,
		opts?: { undo?: (okIds: string[]) => void }
	): Promise<string[]> {
		return runBulkOn(wsSlug, req, verb, opts);
	}

	const idsOf = (items: Item[]) => items.map((i) => i.id);

	// Destructive bulk actions (archive / move) carry an Undo (TASK-1674):
	// archive undoes via the bulk `restore` op; move undoes by moving the
	// items back to their source lane (they all shared the lane's status).
	// Both capture `ws` so a deferred Undo targets the original workspace
	// even after the user navigates away (Codex round 1).
	function handleBulkArchive(items: Item[]) {
		const ws = wsSlug;
		return runBulkOn(ws, { op: 'archive', ids: idsOf(items) }, 'Archived', {
			undo: (okIds) => runBulkOn(ws, { op: 'restore', ids: okIds }, 'Restored')
		});
	}
	function handleBulkMove(items: Item[], status: string) {
		const ws = wsSlug;
		// All lane items shared the lane's status; undo moves them back.
		const sourceStatus = items.length ? String(parseFields(items[0]).status ?? '') : '';
		return runBulkOn(ws, { op: 'move', ids: idsOf(items), status }, 'Moved', {
			undo: sourceStatus
				? (okIds) => runBulkOn(ws, { op: 'move', ids: okIds, status: sourceStatus }, 'Moved back')
				: undefined
		});
	}
	function handleBulkTag(items: Item[], tag: string) {
		return runBulk({ op: 'tag', ids: idsOf(items), tags: [tag] }, 'Tagged');
	}
	function handleBulkUntag(items: Item[], tag: string) {
		return runBulk({ op: 'untag', ids: idsOf(items), tags: [tag] }, 'Untagged');
	}
	function handleBulkSetPriority(items: Item[], priority: string) {
		return runBulk({ op: 'set-priority', ids: idsOf(items), priority }, 'Updated');
	}
	function handleBulkAssign(items: Item[], userId: string) {
		return runBulk({ op: 'assign', ids: idsOf(items), assigned_user_id: userId }, 'Assigned');
	}

	async function handleRestore(item: Item) {
		if (!wsSlug) return;
		const epoch = localIndex.scopeEpochFor(wsSlug);
		try {
			const restored = await api.items.restore(wsSlug, item.id);
			localIndex.upsert(wsSlug, restored, epoch);
			toastStore.show(`Restored "${item.title}"`, 'success');
		} catch {
			toastStore.show('Failed to restore item', 'error');
		}
	}

	// --- Saved views ---
	//
	// Default view persistence (TASK-1366 / Phase 3d). Pure client-side
	// preference stored in localStorage, keyed by (workspace, collection).
	// No schema change, no cross-device sync — the DOC-1342 design
	// explicitly recommends the localStorage path for v1 ("ships faster,
	// no permission semantics to debate"). A server-side `is_default`
	// column can come in a follow-up if cross-device persistence
	// becomes important.

	const DEFAULT_VIEW_KEY = 'pad-default-view';

	function defaultViewKey(ws: string, coll: string): string {
		return `${DEFAULT_VIEW_KEY}:${ws}:${coll}`;
	}

	function readDefaultViewId(ws: string, coll: string): string | null {
		try {
			return localStorage.getItem(defaultViewKey(ws, coll));
		} catch {
			return null;
		}
	}

	function writeDefaultViewId(ws: string, coll: string, id: string | null) {
		try {
			if (id) localStorage.setItem(defaultViewKey(ws, coll), id);
			else localStorage.removeItem(defaultViewKey(ws, coll));
		} catch {
			// Storage unavailable (private mode, quota) — degrade
			// silently. The default stays in-session.
		}
	}

	// Reactive: is the currently active view marked as the user's
	// default for this (ws, coll)? Used to render the "Make default"
	// affordance state. Re-reads localStorage on every render of the
	// affordance — cheap and avoids stale state if another tab flipped
	// the default. Returns null if no view is active.
	let defaultViewId = $state<string | null>(null);
	$effect(() => {
		// Re-read whenever the route or active view changes so the
		// affordance reflects the right state per page entry.
		void wsSlug;
		void collSlug;
		void activeViewId;
		if (!wsSlug || !collSlug) {
			defaultViewId = null;
			return;
		}
		defaultViewId = readDefaultViewId(wsSlug, collSlug);
	});

	let isCurrentDefault = $derived(
		activeViewId !== null && activeViewId === defaultViewId,
	);

	function toggleMakeDefault() {
		if (!wsSlug || !collSlug || !activeViewId) return;
		if (isCurrentDefault) {
			writeDefaultViewId(wsSlug, collSlug, null);
			defaultViewId = null;
			toastStore.show('Removed as default view', 'success');
		} else {
			writeDefaultViewId(wsSlug, collSlug, activeViewId);
			defaultViewId = activeViewId;
			toastStore.show('Set as default view for this collection', 'success');
		}
	}

	// Apply the user's default saved view on collection-page mount.
	// Skip when the URL already carries explicit state (search query,
	// active filters, non-default view mode) — that signals the user
	// arrived from a shared/bookmarked URL and shouldn't be hijacked.
	//
	// CRITICAL: gate on `!metaLoading`. `loadCollection` flips
	// `metaLoading=true` at entry, assigns `savedViews` mid-flight,
	// then calls `loadUrlFilters()` synchronously near the end, and
	// only flips `metaLoading=false` in the `finally` block. Running
	// this effect on a `savedViews` change ALONE has two bugs Codex
	// round 1 caught:
	//   1. Race with URL parsing: savedViews lands before
	//      `loadUrlFilters` does, so `searchQuery` / `activeFilters`
	//      are still stale (empty or from the prior route), and the
	//      effect would overwrite the incoming URL state with the
	//      default view.
	//   2. Cross-collection navigation: after `wsSlug`/`collSlug`
	//      flip, the reset effect zeros `defaultViewApplied` but
	//      `savedViews` still holds the PREVIOUS collection's views
	//      until the new fetch resolves. A `find()` on the wrong list
	//      misses the new default and would erase the localStorage
	//      pointer.
	// Gating on `metaLoading` flipping false guarantees both
	// `savedViews` is for the current collection AND
	// `loadUrlFilters` has applied any incoming URL state.
	let defaultViewApplied = $state(false);
	$effect(() => {
		void wsSlug;
		void collSlug;
		void metaLoading;
		if (!wsSlug || !collSlug) return;
		if (defaultViewApplied) return;
		// Wait for the collection-load cycle to settle entirely.
		if (metaLoading) return;
		// `metaLoading=false` with `savedViews` empty is a legitimate
		// "collection has no saved views yet" state — mark as applied
		// so we don't re-evaluate every render.
		if (savedViews.length === 0) {
			defaultViewApplied = true;
			return;
		}
		// Don't override URL-driven state. Read `page.url` directly —
		// not the parsed `searchQuery` / `activeFilters` state —
		// because:
		//   * `?view=board` doesn't populate either, so a parsed-state
		//     check would miss it. Codex round 2 P1.
		//   * `loadUrlFilters` doesn't clear absent params, so a
		//     parsed-state check might still see leftover values from
		//     the previous route on cross-collection nav and skip a
		//     default that the new clean URL would have allowed.
		//     Codex round 2 P2.
		// Any URL param means user has explicit intent; the only
		// params the collection page writes (`view`, `q`, field
		// filters) are all user-driven, so checking for any is safe.
		const urlOverrides = page.url.searchParams.size > 0;
		if (urlOverrides) {
			defaultViewApplied = true;
			return;
		}
		const id = readDefaultViewId(wsSlug, collSlug);
		if (!id) {
			defaultViewApplied = true;
			return;
		}
		const view = savedViews.find((v) => v.id === id);
		if (view) {
			applyViewConfig(view);
		} else {
			// Stale localStorage pointer — the view was deleted by
			// another tab / on another device. Clean up.
			writeDefaultViewId(wsSlug, collSlug, null);
		}
		defaultViewApplied = true;
	});

	// Reset the one-shot apply gate whenever the route changes so the
	// next collection entry re-evaluates its own default.
	$effect(() => {
		void wsSlug;
		void collSlug;
		defaultViewApplied = false;
	});

	function buildViewConfig(): ViewConfig {
		const config: ViewConfig = {};
		// Defensive mutex enforcement (PLAN-2095 DR-3): the interactive
		// handlers already keep `activeFilters.parent`/`phase` and
		// `unparentedFilter` mutually exclusive, but a saved view should
		// never persist a contradictory combination even if some other
		// path (a stale prop, a future call site) let both linger.
		const effectiveFilters = unparentedEffective(unparentedFilter, unparentedMetadataAvailable)
			? clearParentFilter(activeFilters)
			: activeFilters;
		const filterEntries = Object.entries(effectiveFilters);
		const filters: NonNullable<ViewConfig['filters']> = filterEntries.map(
			([field, value]) => ({ field, op: 'eq', value }),
		);
		// Tags ride along as a single `in` filter (OR semantics) with an
		// array value — the ViewFilter shape already supports `op: 'in'`
		// + an array `value`.
		if (selectedTags.length > 0) {
			filters.push({ field: 'tags', op: 'in', value: [...selectedTags] });
		}
		// "Unparented only" persists as the reserved `$unparented` condition
		// (PLAN-2095 DR-5). Gated on `unparentedMetadataAvailable` (not just
		// intent) so a restricted caller can never save it into a view.
		const unparentedEntry = buildUnparentedViewFilter(unparentedFilter, unparentedMetadataAvailable);
		if (unparentedEntry) {
			filters.push(unparentedEntry);
		}
		if (filters.length > 0) {
			config.filters = filters;
		}
		return config;
	}

	function applyViewConfig(view: View) {
		// Set view mode
		const vt = view.view_type;
		if (vt === 'list' || vt === 'board' || vt === 'table') {
			viewMode = vt;
			saveViewMode(vt);
		}

		// Parse and apply config
		let config: ViewConfig = {};
		try { config = JSON.parse(view.config); } catch {}

		// Apply filters
		const newFilters: Record<string, string> = {};
		let newTags: string[] = [];
		if (config.filters) {
			for (const f of config.filters) {
				if (f.field === 'tags' && f.op === 'in') {
					newTags = Array.isArray(f.value) ? f.value : [f.value];
				} else if (f.op === 'eq' && typeof f.value === 'string') {
					newFilters[f.field] = f.value;
				}
				// The reserved `$unparented` condition (`value: true`, a
				// boolean) never matches the `typeof f.value === 'string'`
				// branch above, so it's naturally skipped here — recovered
				// separately below via `viewHasUnparentedFilter`.
			}
		}
		// Recover "Unparented only" intent from the saved view (PLAN-2095
		// DR-5). Applied as raw intent — same as the URL path — and left to
		// `unparentedApplied` / the restricted-clearing effect to enforce
		// DR-2 once the projection scope is known. This does NOT check
		// `unparentedMetadataAvailable` here because `applyViewConfig` can
		// run before the local index has hydrated (the default-view-on-
		// mount effect gates only on `!metaLoading`); gating here would
		// incorrectly drop the filter for a legitimate unrestricted caller
		// on a cold load.
		const newUnparented = viewHasUnparentedFilter(config.filters);
		// Mutual exclusivity (PLAN-2095 DR-3): a legacy/hand-authored saved
		// view could carry both a specific-parent filter and the
		// unparented pseudo-filter at once — resolve it the same way the
		// interactive chip toggle does (Codex review round 1).
		const resolved = resolveParentUnparentedMutex(newFilters, newUnparented);
		activeFilters = resolved.filters;
		selectedTags = newTags;
		unparentedFilter = resolved.unparented;
		searchQuery = '';
		searchResultRank = null;

		// Open filters panel if the view has filters
		if (Object.keys(resolved.filters).length > 0 || newTags.length > 0 || resolved.unparented) {
			filtersOpen = true;
		}

		activeViewId = view.id;
		updateUrlFilters();
	}

	function clearActiveView() {
		activeViewId = null;
		activeFilters = {};
		selectedTags = [];
		unparentedFilter = false;
		searchQuery = '';
		searchResultRank = null;
		filtersOpen = false;
		updateUrlFilters();
	}

	async function saveCurrentView() {
		const name = saveViewName.trim();
		if (!name || !wsSlug || !collSlug || savingView) return;
		savingView = true;
		try {
			const config = buildViewConfig();
			const view = await api.views.create(wsSlug, collSlug, {
				name,
				view_type: viewMode,
				config: JSON.stringify(config)
			});
			savedViews = [...savedViews, view];
			activeViewId = view.id;
			saveViewOpen = false;
			saveViewName = '';
			toastStore.show(`Saved view "${name}"`, 'success');
		} catch {
			toastStore.show('Failed to save view', 'error');
		} finally {
			savingView = false;
		}
	}

	async function deleteView(viewId: string, viewName: string) {
		if (!wsSlug || !collSlug) return;
		try {
			await api.views.delete(wsSlug, collSlug, viewId);
			savedViews = savedViews.filter((v) => v.id !== viewId);
			if (activeViewId === viewId) {
				clearActiveView();
			}
			// Clear the persisted default if it pointed at the
			// just-deleted view — otherwise the next entry to this
			// collection would re-read a dangling pointer and silently
			// no-op. TASK-1366.
			if (defaultViewId === viewId) {
				writeDefaultViewId(wsSlug, collSlug, null);
				defaultViewId = null;
			}
			toastStore.show(`Deleted view "${viewName}"`, 'success');
		} catch {
			toastStore.show('Failed to delete view', 'error');
		}
	}

	function openSaveView() {
		saveViewOpen = true;
		requestAnimationFrame(() => saveViewInput?.focus());
	}

	function handleSaveViewKeydown(e: KeyboardEvent) {
		if (e.key === 'Enter' && saveViewName.trim()) {
			e.preventDefault();
			saveCurrentView();
		} else if (e.key === 'Escape') {
			saveViewOpen = false;
			saveViewName = '';
		}
	}

	function formatLabel(value: string): string {
		return value.replace(/_/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase());
	}
</script>

<svelte:window onkeydown={handlePageKeydown} />

<!--
	Split-pane layout (PLAN-2105 / TASK-2112). When `?item=` is set the page
	becomes a flex row: the list column (flex:1, always mounted) + a right-
	docked <ItemDetail embedded> pane. The list content is always wrapped in
	.list-column so opening/closing the pane never remounts the list. The pane
	is the ONLY thing that mounts/unmounts on open/close — see the NO-{#key}
	note on the ItemDetail mount below.
-->
<div class="collection-page" class:board-active={viewMode === 'board'} class:pane-open={!!openItemRef}>
	<div class="list-column">
	{#if loading}
		<div class="loading">Loading...</div>
	{:else if metaError}
		<!-- Transient collection-metadata load failure (network / 5xx /
		     429) — NOT a genuine 404 (BUG-2025). Show an error + Retry
		     path instead of the misleading "Collection not found" empty
		     state, mirroring the items branch's retry box. -->
		<div class="empty-state-box">
			<div class="empty-icon">⚠️</div>
			<h2>Couldn't load this collection</h2>
			<p>Something went wrong while loading. It may be a temporary network or server issue.</p>
			<button
				class="empty-cta"
				onclick={() => {
					if (wsSlug && collSlug) loadCollection(wsSlug, collSlug, showArchived);
				}}
			>
				Retry
			</button>
		</div>
	{:else if !collection}
		<div class="empty-state">Collection not found</div>
	{:else}
		<!-- Header -->
		<div class="page-header">
			<div class="title-row">
				<div class="title-group">
					<h1>
						{#if collection.icon}<span class="collection-icon">{collection.icon}</span>{/if}
						{collection.name}
						<span class="item-count">{items.length}</span>
					</h1>

					<!-- Realtime stream status (PLAN-1984 / TASK-2027): surfaces the
					     already-computed SSE connection state so a stale board hints
					     when live updates are down or unauthorized. -->
					<SSEStatusIndicator />
				</div>

				<div class="header-actions">
					{#if viewport.isMobile}
						<!--
							Mobile: labeled chip + BottomSheet picker. Icon-only
							segmented buttons are hard to decode on touch, and the
							sheet gives each option a clear label.
						-->
						<button
							class="view-chip"
							type="button"
							onclick={() => (viewSheetOpen = true)}
							aria-label="Change view"
						>
							<span class="view-chip-label">View: {viewModeLabel}</span>
							<span class="view-chip-caret" aria-hidden="true">▾</span>
						</button>
						{#if viewSheetOpen}
							<!--
								Gate the sheet on `viewSheetOpen` (gate-on-open
								pattern from TASK-633) so BottomSheet's global
								keydown listener isn't mounted when idle.
							-->
							<BottomSheet
								open={viewSheetOpen}
								onclose={() => (viewSheetOpen = false)}
								title="Choose view"
							>
								<div class="view-sheet-body">
									<button
										class="view-sheet-option"
										class:active={viewMode === 'list'}
										type="button"
										onclick={() => selectViewMode('list')}
									>
										<span class="view-sheet-icon">&#9776;</span>
										<span>List</span>
									</button>
									<button
										class="view-sheet-option"
										class:active={viewMode === 'board'}
										type="button"
										onclick={() => selectViewMode('board')}
									>
										<span class="view-sheet-icon">&#9638;</span>
										<span>Board</span>
									</button>
									<button
										class="view-sheet-option"
										class:active={viewMode === 'table'}
										type="button"
										onclick={() => selectViewMode('table')}
									>
										<span class="view-sheet-icon">&#9783;</span>
										<span>Table</span>
									</button>
								</div>
							</BottomSheet>
						{/if}
					{:else}
						<div class="view-toggle">
							<button
								class="toggle-btn"
								class:active={viewMode === 'list'}
								onclick={() => { saveViewMode('list'); updateUrlFilters(); }}
								aria-label="List view"
								title="List view"
							>&#9776;</button>
							<button
								class="toggle-btn"
								class:active={viewMode === 'board'}
								onclick={() => { saveViewMode('board'); updateUrlFilters(); }}
								aria-label="Board view"
								title="Board view"
							>&#9638;</button>
							<button
								class="toggle-btn"
								class:active={viewMode === 'table'}
								onclick={() => { saveViewMode('table'); updateUrlFilters(); }}
								aria-label="Table view"
								title="Table view"
							>&#9783;</button>
						</div>
					{/if}

					{#if viewMode !== 'table'}
						<label class="sort-control" title="Sort items">
							<span class="sort-label">Sort</span>
							<select
								class="sort-select"
								value={sortMode}
								onchange={(e) => saveSortMode(e.currentTarget.value as SortMode)}
								aria-label="Sort items"
							>
								{#each sortOptions as opt (opt.value)}
									<option value={opt.value}>{opt.label}</option>
								{/each}
							</select>
						</label>
					{/if}

					<button
						class="filter-toggle-btn"
						class:has-filters={hasActiveFilters}
						onclick={() => filtersOpen = !filtersOpen}
						aria-label="Toggle filters"
						title="Toggle filters"
					>
						<svg class="filter-icon" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polygon points="22 3 2 3 10 12.46 10 19 14 21 14 12.46 22 3"/></svg>
						<span class="filter-label">Filters</span>
						{#if hasActiveFilters}
							<span class="filter-badge"></span>
						{/if}
					</button>

					<label class="archive-toggle">
						<input type="checkbox" bind:checked={showArchived} />
						<span>Archived</span>
					</label>

					<button
						class="save-view-btn"
						onclick={openSaveView}
						aria-label="Save current view"
						title="Save current view"
					>
						<span class="save-view-icon">&#9733;</span>
						<span class="save-view-label">Save View</span>
					</button>

					{#if collection && (quickActions.length > 0 || isOwner)}
						<QuickActionsMenu
							actions={quickActions}
							{collection}
							scope="collection"
							{wsSlug}
							canEdit={isOwner}
							onmanage={() => {
								editCollectionSection = 'actions';
								editCollectionOpen = true;
							}}
							oncollectionupdated={(updated) => {
								// Apply the returned collection immediately so a fast
								// follow-up save reads fresh settings.quick_actions
								// instead of stale ones — without this, a second
								// save can overwrite the first before loadCollection
								// resolves.
								collection = updated;
								loadCollection(wsSlug, collSlug, showArchived);
							}}
						/>
					{/if}

					{#if isOwner}
						<button
							class="edit-collection-btn"
							onclick={() => { editCollectionOpen = true; }}
							title="Edit collection"
						>
							<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7"/><path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z"/></svg>
							<span class="edit-collection-label">Edit</span>
						</button>
					{/if}

					{#if isOwner}
						<button
							class="share-btn-header"
							onclick={() => { shareDialogOpen = true; }}
							title="Share collection"
						>
							<svg class="share-icon" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M4 12v8a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-8"/><polyline points="16 6 12 2 8 6"/><line x1="12" y1="2" x2="12" y2="15"/></svg>
							<span class="share-btn-label">Share</span>
						</button>
					{/if}

					{#if canEditThisCollection}
						<button class="new-btn" onclick={handleNewButtonClick} disabled={creatingNew}>
							+ <span class="new-btn-label">New {singularName()}</span>
						</button>
					{/if}
				</div>
			</div>

			{#if filtersOpen}
				<div class="filters-panel">
					<FilterBar
						{collection}
						{activeFilters}
						{searchQuery}
						onFilterChange={handleFilterChange}
						onSearchChange={handleSearchChange}
						{relationLabels}
						{tagCounts}
						{selectedTags}
						onTagFilterChange={handleTagFilterChange}
						bind:searchInputEl
						unparentedAvailable={unparentedMetadataAvailable}
						unparentedActive={unparentedApplied}
						onUnparentedChange={handleUnparentedChange}
					/>
				</div>
			{/if}

			{#if savedViews.length > 0}
				<div class="saved-views-bar">
					<button
						class="saved-view-tab"
						class:active={activeViewId === null}
						onclick={clearActiveView}
					>All</button>
					{#each savedViews as view (view.id)}
						<button
							class="saved-view-tab"
							class:active={activeViewId === view.id}
							onclick={() => applyViewConfig(view)}
						>
							<span class="saved-view-name">{view.name}</span>
							{#if defaultViewId === view.id}
								<!--
									Pin icon marks the saved default. Visible
									on every tab (not just active) so users can
									see at a glance which view will be applied
									on next entry. TASK-1366.
								-->
								<span class="saved-view-default" title="Default view — applied on entry" aria-label="Default view">📌</span>
							{/if}
							<span
								class="saved-view-delete"
								role="button"
								tabindex="0"
								onclick={(e) => { e.stopPropagation(); deleteView(view.id, view.name); }}
								onkeydown={(e) => { if (e.key === 'Enter') { e.stopPropagation(); deleteView(view.id, view.name); } }}
								aria-label="Delete view {view.name}"
								title="Delete view"
							>&times;</span>
						</button>
					{/each}
					{#if activeViewId !== null}
						<!--
							"Make default" affordance (TASK-1366 / Phase 3d).
							Only shown when a saved view is active — toggles
							whether THIS view becomes the per-(workspace,
							collection) default applied on next page entry.
							Storage is pure localStorage v1; cross-device
							syncs would need a server-side is_default column
							(out of scope for this phase).
						-->
						<button
							class="saved-view-default-toggle"
							class:active={isCurrentDefault}
							onclick={toggleMakeDefault}
							title={isCurrentDefault
								? 'Remove as default for this collection'
								: 'Apply this view automatically on next entry'}
						>
							{isCurrentDefault ? 'Default ★' : 'Make default'}
						</button>
					{/if}
				</div>
			{/if}

			{#if saveViewOpen}
				<div class="save-view-form">
					<input
						bind:this={saveViewInput}
						bind:value={saveViewName}
						class="save-view-input"
						placeholder="View name — press Enter to save, Esc to cancel"
						onkeydown={handleSaveViewKeydown}
						onblur={() => { if (!saveViewName.trim()) saveViewOpen = false; }}
						disabled={savingView}
					/>
				</div>
			{/if}

			{#if quickCreateOpen && canEditThisCollection}
				<div class="quick-create">
					<input
						bind:this={quickCreateInput}
						bind:value={quickCreateTitle}
						class="quick-create-input"
						placeholder="Title — press Enter to create, Esc to cancel"
						onkeydown={handleQuickCreateKeydown}
						onblur={() => { if (!quickCreateTitle.trim()) quickCreateOpen = false; }}
						disabled={creatingNew}
					/>
				</div>
			{/if}

			<div class="header-separator"></div>
		</div>

		<!-- Content -->
		{#if (indexError || deltaSyncFailed) && items.length === 0}
			<!-- localIndex bootstrap failed and the cache is empty
			     (e.g. transient /items-index failure on cold load,
			     or auth revoked on /items-changes). Show a retry
			     path instead of the misleading "No items yet" empty
			     state OR a stuck-forever "Loading…" spinner. -->
			<div class="empty-state-box">
				<div class="empty-icon">⚠️</div>
				<h2>Couldn't load {collection.name.toLowerCase()}</h2>
				<p>Something went wrong while loading this workspace.</p>
				<button
					class="empty-cta"
					onclick={() => {
						deltaSyncFailed = false;
						localIndex.reset(wsSlug);
						localIndex.bootstrap(wsSlug, { userId: authStore.userId || null });
					}}
				>
					Retry
				</button>
			</div>
		{:else if items.length === 0}
			<div class="empty-state-box">
				<div class="empty-icon">{collection.icon || '📦'}</div>
				<h2>No {collection.name.toLowerCase()} yet</h2>
				{#if canEditThisCollection}
					<p>Create your first {singularName().toLowerCase()} to get started.</p>
					<button class="empty-cta" onclick={openQuickCreate}>+ Create {singularName()}</button>
					{#if emptyHint}
						<p class="empty-hint">Or try: <code>{emptyHint}</code></p>
					{/if}
				{:else}
					<p>This collection is empty.</p>
				{/if}
			</div>
		{:else if filteredItems.length === 0 && (searchQuery || Object.keys(activeFilters).length > 0 || unparentedApplied)}
			<div class="empty-state-box">
				<div class="empty-icon">🔍</div>
				<h2>No matches</h2>
				<p>No items match your current filters.
					<button class="clear-link" onclick={() => { activeFilters = {}; searchQuery = ''; searchResultRank = null; unparentedFilter = false; updateUrlFilters(); }}>Clear filters</button>
				</p>
			</div>
		{:else if viewMode === 'board'}
			<BoardView
				items={filteredItems}
				{collection}
				{wsSlug}
				{groupField}
				{focusedItemId}
				onStatusChange={handleStatusChange}
				onReorder={handleReorder}
				onArchiveColumn={canBulkEdit ? handleBulkArchive : undefined}
				onGroupReorder={handleGroupReorder}
				oncreate={canEditThisCollection ? openQuickCreate : undefined}
				onCreateInColumn={canEditThisCollection ? quickCreateInColumn : undefined}
				bind:draftText
				bind:draftOpen
				onMoveColumn={canBulkEdit ? handleBulkMove : undefined}
				onTagColumn={canBulkEdit ? handleBulkTag : undefined}
				onUntagColumn={canBulkEdit ? handleBulkUntag : undefined}
				onSetPriorityColumn={canBulkEdit ? handleBulkSetPriority : undefined}
				onAssignColumn={canBulkEdit ? handleBulkAssign : undefined}
				members={workspaceMembers}
				{tagSuggestions}
				filtered={hasActiveFilters}
				{itemProgress}
				{progressLabel}
				canEdit={canEditThisCollection}
				preserveOrder={searchQuery.trim() !== ''}
				{sortMode}
				onItemOpen={openItemPane}
			/>
		{:else if viewMode === 'table'}
			<TableView
				items={filteredItems}
				{collection}
				{wsSlug}
				{focusedItemId}
				onStatusChange={handleStatusChange}
				onReorder={handleReorder}
				oncreate={canEditThisCollection ? openQuickCreate : undefined}
				{itemProgress}
				{progressLabel}
				canEdit={canEditThisCollection}
				preserveOrder={searchQuery.trim() !== ''}
				{sortMode}
				onItemOpen={openItemPane}
			/>
		{:else}
			<ListView
				items={filteredItems}
				{collection}
				{wsSlug}
				{groupField}
				{focusedItemId}
				{statusOptions}
				onStatusChange={handleStatusChange}
				onReorder={handleReorder}
				onArchiveGroup={canBulkEdit ? handleBulkArchive : undefined}
				onGroupReorder={handleGroupReorder}
				oncreate={canEditThisCollection ? openQuickCreate : undefined}
				{itemProgress}
				{progressLabel}
				canEdit={canEditThisCollection}
				preserveOrder={searchQuery.trim() !== ''}
				{sortMode}
				onItemOpen={openItemPane}
			/>
		{/if}
	{/if}
	</div>
	{#if openItemRef}
		<!-- Draggable divider (PLAN-2105 / TASK-2114). Pointer-captured so a
		     drag stays on the handle (never selects text or fires a row-click)
		     and also keyboard-adjustable via the arrow keys. This is the ARIA
		     "window splitter" pattern — a focusable separator IS interactive,
		     but Svelte's a11y heuristic treats every separator as
		     non-interactive, so the tabindex + pointer/key handlers are
		     legitimately ignored here. -->
		<!-- svelte-ignore a11y_no_noninteractive_tabindex -->
		<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
		<div
			class="pane-divider"
			class:resizing={resizingPane}
			role="separator"
			tabindex="0"
			aria-orientation="vertical"
			aria-label="Resize detail pane"
			aria-valuemin={ariaMin}
			aria-valuemax={ariaMax}
			aria-valuenow={Math.min(ariaMax, Math.max(ariaMin, Math.round(paneWidth ?? measuredPaneWidth ?? PANE_WIDTH_DEFAULT)))}
			onpointerdown={onDividerPointerDown}
			onpointermove={onDividerPointerMove}
			onpointerup={endPaneResize}
			onlostpointercapture={endPaneResize}
			onkeydown={onDividerKeydown}
		></div>
		<!--
			The detail pane. CRITICAL (PLAN-2105 / TASK-2112): NO {#key} wrapper.
			A→B item switch must be a PROP UPDATE (`ref` change re-drives
			ItemDetail's loadData + collabKey via its own reactive effects,
			reusing the mounted instance + its single SSE subscription). A
			{#key} would silently full-remount on every switch and defeat the
			whole design. Open/close (this {#if}) is the ONLY mount/unmount.
			`onNavigateAway` handles the collection-rename case; onClose/onGone
			clear only `?item=`, preserving view/sort/filter/tags/search.

			`--pane-width` carries the persisted width (TASK-2114); undefined
			until localStorage is read so the CSS clamp() default applies on
			first paint when there's no stored value.
		-->
		<aside
			class="item-pane"
			bind:this={paneEl}
			style={paneWidth != null ? `--pane-width: ${paneWidth}px` : undefined}
		>
			<ItemDetail
				ref={openItemRef}
				embedded
				{username}
				{wsSlug}
				{collSlug}
				onClose={closeItemPane}
				onGone={closeItemPane}
				onNavigateAway={(url) => goto(url)}
			/>
		</aside>
	{/if}
</div>

{#if isOwner && collection}
	<ShareDialog
		{wsSlug}
		type="collection"
		targetSlug={collection.slug}
		targetName={collection.name}
		bind:open={shareDialogOpen}
	/>
{/if}

{#if isOwner && collection}
	<EditCollectionModal
		bind:open={editCollectionOpen}
		{collection}
		{wsSlug}
		initialSection={editCollectionSection}
		onupdated={(updated) => {
			collectionStore.loadCollections(wsSlug);
			if (updated && updated.slug !== collSlug) {
				goto(`/${username}/${wsSlug}/${updated.slug}`);
			} else {
				loadCollection(wsSlug, collSlug, showArchived);
			}
		}}
		onclose={() => {
			editCollectionOpen = false;
			editCollectionSection = undefined;
		}}
	/>
{/if}

<!-- Leave guard (TASK-1676): an in-app navigation was intercepted because a lane
     has an unsaved draft card. Backdrop/Escape dismiss maps to "Stay" (the safe,
     non-destructive default) via the shared Modal primitive (TASK-2083). -->
<Modal
	open={showLeaveDialog}
	onclose={leaveStay}
	ariaLabel="Unsaved card"
	maxWidth="360px"
	placement="center"
	--modal-bg="var(--bg-primary)"
	--modal-shadow="var(--shadow-lg, 0 10px 30px rgba(0, 0, 0, 0.3))"
>
	{#if showLeaveDialog}
		<div class="leave-dialog">
			<h3 class="leave-title">Unsaved card</h3>
			<p class="leave-body">You have an unsaved card. Save it before leaving?</p>
			<div class="leave-actions">
				<button class="leave-stay" disabled={savingDrafts} onclick={leaveStay}>Stay</button>
				<button class="leave-discard" disabled={savingDrafts} onclick={leaveDiscard}>Discard</button>
				<button class="leave-save" disabled={savingDrafts} onclick={leaveSaveAll}>Save</button>
			</div>
		</div>
	{/if}
</Modal>

<style>
	/* Surface/backdrop/Escape come from the shared <Modal> primitive (TASK-2083);
	   this wrapper just restores the inner padding. */
	.leave-dialog {
		padding: var(--space-4);
	}
	.leave-title {
		margin: 0 0 var(--space-2);
		font-size: 1em;
		color: var(--text-primary);
	}
	.leave-body {
		margin: 0 0 var(--space-4);
		font-size: 0.875em;
		color: var(--text-secondary);
	}
	.leave-actions {
		display: flex;
		justify-content: flex-end;
		gap: var(--space-2);
	}
	.leave-actions button {
		padding: 6px 14px;
		border-radius: var(--radius-sm);
		font-size: 0.8125em;
		cursor: pointer;
		border: 1px solid var(--border);
	}
	.leave-actions button:disabled {
		opacity: 0.5;
		cursor: default;
	}
	.leave-stay {
		background: var(--bg-secondary);
		color: var(--text-primary);
	}
	.leave-discard {
		background: none;
		border-color: var(--accent-red, #ef4444) !important;
		color: var(--accent-red, #ef4444);
	}
	.leave-save {
		background: var(--accent-blue);
		border-color: var(--accent-blue) !important;
		color: #fff;
	}

	.collection-page {
		max-width: var(--content-max-width);
		margin: 0 auto;
		padding: var(--space-8) var(--space-6);
	}

	.collection-page.board-active {
		max-width: none;
		padding: var(--space-6) var(--space-6);
		/*
			Fill the scrollable .main-content region, NOT the viewport.
			.main-content (root +layout) already sizes itself below the
			workspace TopBar via flexbox, so `height: 100%` resolves to
			exactly the available space whether the TopBar is shown or
			hidden. The old `height: 100vh` claimed the full viewport and
			overflowed .main-content by the TopBar's height whenever the
			bar was visible — the extra scrollable space in BUG-1844.
		*/
		height: 100%;
		display: flex;
		flex-direction: column;
		overflow: hidden;
	}
	.board-active .page-header {
		flex-shrink: 0;
	}

	/* ── Split-pane layout (PLAN-2105 / TASK-2112) ──────────────────────
	   The list content is always wrapped in .list-column so the detail
	   pane can mount/unmount without remounting the list. In the default
	   (no-pane, non-board) layout .list-column is a transparent block, so
	   no styles are needed there. Board view is a fixed-height flex column
	   though, so the wrapper must fill it: the board's own `flex:1;
	   min-height:0` expects its parent to be a constrained flex column,
	   which .list-column now sits in place of. */
	.collection-page.board-active .list-column {
		flex: 1;
		min-height: 0;
		display: flex;
		flex-direction: column;
		overflow: hidden;
	}

	/* When a detail pane is open the page becomes a flex row: list column
	   (flex:1) + a right-docked pane. Break out of the max-width constraint
	   (mirroring board-active) and fill .main-content so the pane docks
	   full-height and scrolls independently of the list. These rules follow
	   .board-active in source order so, for a board+pane combination (equal
	   specificity), the row layout wins the shared props. */
	.collection-page.pane-open {
		max-width: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: row;
		align-items: stretch;
		height: 100%;
		overflow: hidden;
	}
	.collection-page.pane-open .list-column {
		flex: 1 1 0;
		min-width: 0;
		overflow-y: auto;
		padding: var(--space-8) var(--space-6);
	}
	/* Board manages its own internal height + horizontal scroll, so keep
	   the column clipped and let the board fill it. */
	.collection-page.pane-open.board-active .list-column {
		overflow: hidden;
		padding: var(--space-6);
	}
	.item-pane {
		/* Width comes from the persisted `--pane-width` CSS var (TASK-2114);
		   the clamp() is the fallback before localStorage is read / when no
		   width was ever stored. The .pane-divider (not this border) is the
		   resting separator on desktop. */
		flex: 0 0 var(--pane-width, clamp(360px, 38%, 640px));
		min-width: 0;
		overflow-y: auto;
		background: var(--bg-primary);
	}

	/* Draggable resize handle between the list column and the pane
	   (PLAN-2105 / TASK-2114). The 6px-wide element is the grab target; a
	   centered pseudo-element draws the resting 1px separator (replacing the
	   pane's old border-left) and thickens to an accent line on hover, focus,
	   or active drag. */
	.pane-divider {
		flex: 0 0 6px;
		align-self: stretch;
		position: relative;
		background: transparent;
		cursor: col-resize;
		/* No native text selection or touch scroll/gesture while dragging. */
		user-select: none;
		touch-action: none;
	}
	.pane-divider:focus-visible {
		outline: none;
	}
	.pane-divider::after {
		content: '';
		position: absolute;
		top: 0;
		bottom: 0;
		left: 50%;
		width: 1px;
		transform: translateX(-50%);
		background: var(--border);
		transition: background 0.12s, width 0.12s;
	}
	.pane-divider:hover::after,
	.pane-divider:focus-visible::after,
	.pane-divider.resizing::after {
		background: var(--accent-blue);
		width: 2px;
	}

	@media (max-width: 768px) {
		/* Mobile full-screen overlay is Phase 4 (PLAN-2105); until then the
		   pane stacks below the list so neither column gets crushed. */
		.collection-page.pane-open {
			flex-direction: column;
			overflow-y: auto;
		}
		.collection-page.pane-open .list-column {
			overflow-y: visible;
		}
		/* The vertical resize handle is meaningless in the stacked layout. */
		.pane-divider {
			display: none;
		}
		.item-pane {
			flex: 1 1 auto;
			border-top: 1px solid var(--border);
		}
	}

	.loading {
		text-align: center;
		padding-top: 20vh;
		color: var(--text-muted);
	}

	.empty-state-box {
		text-align: center;
		padding: var(--space-10) var(--space-6);
		color: var(--text-secondary);
	}
	.empty-icon {
		font-size: 3em;
		margin-bottom: var(--space-4);
		opacity: 0.6;
	}
	.empty-state-box h2 {
		font-size: 1.2em;
		font-weight: 600;
		margin: 0 0 var(--space-2) 0;
		color: var(--text-primary);
	}
	.empty-state-box p {
		font-size: 0.9em;
		color: var(--text-muted);
		margin: 0 0 var(--space-5) 0;
	}
	.empty-hint {
		font-size: 0.82em !important;
		color: var(--text-muted) !important;
		margin-top: var(--space-3) !important;
	}
	.empty-hint code {
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: 3px;
		padding: 1px 5px;
		font-size: 0.95em;
	}
	.empty-cta {
		display: inline-block;
		background: var(--accent-blue);
		color: #fff;
		padding: var(--space-2) var(--space-5);
		border-radius: var(--radius);
		font-weight: 600;
		font-size: 0.9em;
		text-decoration: none;
		transition: opacity 0.1s;
	}
	.empty-cta:hover { opacity: 0.85; }
	.clear-link {
		color: var(--accent-blue);
		background: none;
		border: none;
		cursor: pointer;
		font-size: inherit;
		text-decoration: underline;
	}

	/* Header */
	.page-header {
		margin-bottom: var(--space-4);
	}

	.title-row {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-4);
		margin-bottom: var(--space-3);
		flex-wrap: wrap;
	}

	.title-group {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		flex-wrap: wrap;
		min-width: 0;
	}

	h1 {
		font-size: 1.6em;
		margin: 0;
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}

	.collection-icon {
		font-size: 0.9em;
	}

	.item-count {
		font-size: 0.5em;
		font-weight: 400;
		color: var(--text-muted);
		background: var(--bg-tertiary);
		padding: 2px 8px;
		border-radius: 10px;
		vertical-align: middle;
	}

	.header-actions {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		flex-wrap: wrap;
	}

	.view-toggle {
		display: flex;
		border: 1px solid var(--border);
		border-radius: var(--radius);
		overflow: hidden;
		flex-shrink: 0;
	}

	.toggle-btn {
		background: var(--bg-secondary);
		border: none;
		padding: var(--space-1) var(--space-2);
		cursor: pointer;
		font-size: 0.95em;
		color: var(--text-secondary);
		line-height: 1;
	}

	.toggle-btn:not(:last-child) {
		border-right: 1px solid var(--border);
	}

	.toggle-btn.active {
		background: var(--bg-tertiary);
		color: var(--text-primary);
	}

	.toggle-btn:hover:not(.active) {
		background: var(--bg-hover);
	}

	/* Mobile view chip — replaces the segmented .view-toggle under 640px.
	   The chip shows a labeled summary ("View: Board ▾") that opens a
	   BottomSheet of labeled choices. */
	.view-chip {
		display: inline-flex;
		align-items: center;
		gap: var(--space-1);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-1) var(--space-3);
		font-size: 0.85em;
		color: var(--text-primary);
		cursor: pointer;
		flex-shrink: 0;
	}

	.view-chip:hover {
		border-color: var(--accent-blue);
	}

	.view-chip-caret {
		color: var(--text-muted);
		font-size: 0.9em;
		line-height: 1;
	}

	.view-sheet-body {
		display: flex;
		flex-direction: column;
		padding: 0 var(--space-2) var(--space-3);
	}

	.view-sheet-option {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		width: 100%;
		text-align: left;
		background: none;
		border: none;
		padding: var(--space-3);
		color: var(--text-primary);
		font-size: 1em;
		cursor: pointer;
		border-radius: var(--radius-sm);
	}

	.view-sheet-option:hover {
		background: var(--bg-hover);
	}

	.view-sheet-option.active {
		background: var(--bg-tertiary);
		font-weight: 600;
	}

	.view-sheet-icon {
		font-size: 1.1em;
		width: 1.5em;
		text-align: center;
		color: var(--text-secondary);
	}

	/* Filter toggle */
	.filter-toggle-btn {
		display: flex;
		align-items: center;
		gap: var(--space-1);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-1) var(--space-3);
		cursor: pointer;
		font-size: 0.82em;
		color: var(--text-secondary);
		white-space: nowrap;
		position: relative;
		transition: border-color 0.15s, color 0.15s;
	}

	.filter-toggle-btn:hover {
		color: var(--text-primary);
		border-color: var(--text-muted);
	}

	.filter-toggle-btn.has-filters {
		border-color: var(--accent-blue);
		color: var(--text-primary);
	}

	.filter-icon {
		flex-shrink: 0;
	}

	.filter-badge {
		width: 6px;
		height: 6px;
		border-radius: 50%;
		background: var(--accent-blue);
		flex-shrink: 0;
	}

	.filters-panel {
		padding: var(--space-3) 0;
	}

	/* Page-wide sort control (TASK-1670) — sits next to the view toggle. */
	.sort-control {
		display: flex;
		align-items: center;
		gap: var(--space-1);
		font-size: 0.82em;
		color: var(--text-muted);
		white-space: nowrap;
		flex-shrink: 0;
	}

	.sort-label {
		display: none;
	}

	@media (min-width: 900px) {
		.sort-label {
			display: inline;
		}
	}

	.sort-select {
		appearance: auto;
		background: var(--bg-secondary);
		color: var(--text-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		padding: 4px 6px;
		font-size: inherit;
		cursor: pointer;
	}

	.archive-toggle {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		font-size: 0.82em;
		color: var(--text-muted);
		cursor: pointer;
		white-space: nowrap;
		flex-shrink: 0;
	}

	.archive-toggle input {
		accent-color: var(--accent-blue);
	}

	.archive-toggle:hover {
		color: var(--text-secondary);
	}

	.new-btn {
		background: var(--accent-blue);
		color: #fff;
		padding: var(--space-1) var(--space-4);
		border-radius: var(--radius);
		font-size: 0.85em;
		font-weight: 500;
		text-decoration: none;
		white-space: nowrap;
		flex-shrink: 0;
		transition: opacity 0.1s;
	}

	.new-btn:hover {
		opacity: 0.85;
		text-decoration: none;
	}

	.header-separator {
		height: 1px;
		background: var(--border);
		margin-top: var(--space-2);
	}

	.quick-create {
		margin-top: var(--space-3);
	}

	.quick-create-input {
		width: 100%;
		padding: var(--space-3) var(--space-4);
		background: var(--bg-secondary);
		border: 1px solid var(--accent-blue);
		border-radius: var(--radius);
		color: var(--text-primary);
		font-size: 0.95em;
		outline: none;
		transition: border-color 0.15s;
	}

	.quick-create-input::placeholder {
		color: var(--text-muted);
	}

	.quick-create-input:focus {
		border-color: var(--accent-blue);
		box-shadow: 0 0 0 2px color-mix(in srgb, var(--accent-blue) 15%, transparent);
	}

	/* Save view button */
	.save-view-btn {
		display: flex;
		align-items: center;
		gap: var(--space-1);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-1) var(--space-3);
		cursor: pointer;
		font-size: 0.82em;
		color: var(--text-secondary);
		white-space: nowrap;
		transition: border-color 0.15s, color 0.15s;
	}

	.save-view-btn:hover {
		color: var(--text-primary);
		border-color: var(--text-muted);
	}

	.save-view-icon {
		font-size: 1em;
		line-height: 1;
	}

	/* Saved views tabs */
	.saved-views-bar {
		display: flex;
		align-items: center;
		gap: var(--space-1);
		padding: var(--space-2) 0;
		overflow-x: auto;
		scrollbar-width: none;
	}

	.saved-views-bar::-webkit-scrollbar {
		display: none;
	}

	.saved-view-tab {
		display: flex;
		align-items: center;
		gap: var(--space-1);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-1) var(--space-3);
		cursor: pointer;
		font-size: 0.8em;
		color: var(--text-secondary);
		white-space: nowrap;
		transition: all 0.15s;
	}

	.saved-view-tab:hover {
		background: var(--bg-hover);
		color: var(--text-primary);
	}

	.saved-view-tab.active {
		background: var(--bg-tertiary);
		border-color: var(--accent-blue);
		color: var(--text-primary);
		font-weight: 600;
	}

	.saved-view-delete {
		display: none;
		font-size: 1.1em;
		line-height: 1;
		color: var(--text-muted);
		cursor: pointer;
		padding: 0 2px;
		border-radius: 2px;
	}

	.saved-view-tab:hover .saved-view-delete {
		display: inline;
	}

	.saved-view-delete:hover {
		color: var(--text-primary);
		background: var(--bg-tertiary);
	}

	/* Pin icon on the default saved view (TASK-1366). */
	.saved-view-default {
		font-size: 0.75em;
		line-height: 1;
		opacity: 0.85;
	}

	/*
		"Make default" affordance, only rendered when a saved view is
		active. Visually subordinate to the tabs themselves — a small
		text button that flips to filled state when the current view is
		the persisted default.
	*/
	.saved-view-default-toggle {
		display: inline-flex;
		align-items: center;
		gap: 4px;
		font-size: 0.75em;
		padding: 2px 8px;
		border-radius: 999px;
		color: var(--text-muted);
		background: none;
		border: 1px solid var(--border);
		cursor: pointer;
		white-space: nowrap;
		transition: all 0.15s ease;
		margin-left: var(--space-1);
	}
	.saved-view-default-toggle:hover {
		color: var(--text-secondary);
		background: var(--bg-hover);
	}
	.saved-view-default-toggle.active {
		color: var(--accent-amber);
		border-color: color-mix(in srgb, var(--accent-amber) 40%, transparent);
		background: color-mix(in srgb, var(--accent-amber) 12%, transparent);
	}

	/* Save view form */
	.save-view-form {
		padding: var(--space-2) 0;
	}

	.save-view-input {
		width: 100%;
		max-width: 320px;
		padding: var(--space-2) var(--space-3);
		background: var(--bg-secondary);
		border: 1px solid var(--accent-blue);
		border-radius: var(--radius);
		color: var(--text-primary);
		font-size: 0.85em;
		outline: none;
		transition: border-color 0.15s;
	}

	.save-view-input::placeholder {
		color: var(--text-muted);
	}

	.save-view-input:focus {
		border-color: var(--accent-blue);
		box-shadow: 0 0 0 2px color-mix(in srgb, var(--accent-blue) 15%, transparent);
	}

	@media (max-width: 768px) {
		/* The collection name now lives in the persistent top bar
		   (MobileContextBar, IDEA-1835) — drop the duplicate in-page heading
		   but keep the view controls in the row. */
		.title-row h1 {
			display: none;
		}

		.title-row {
			flex-direction: column;
			align-items: flex-start;
			gap: var(--space-3);
		}

		.header-actions {
			width: 100%;
			justify-content: flex-start;
		}

		.archive-toggle {
			display: none;
		}

		.filter-label {
			display: none;
		}

		.new-btn-label {
			display: none;
		}

		.save-view-label {
			display: none;
		}

		.share-btn-label {
			display: none;
		}

		.edit-collection-label {
			display: none;
		}
	}

	/* Share button */
	.share-btn-header {
		display: flex;
		align-items: center;
		gap: var(--space-1);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-1) var(--space-3);
		cursor: pointer;
		font-size: 0.82em;
		color: var(--text-secondary);
		white-space: nowrap;
		transition: border-color 0.15s, color 0.15s;
	}

	.share-btn-header:hover {
		color: var(--text-primary);
		border-color: var(--text-muted);
	}

	.share-icon {
		flex-shrink: 0;
	}

	/* Edit collection button */
	.edit-collection-btn {
		display: flex;
		align-items: center;
		gap: var(--space-1);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-1) var(--space-3);
		cursor: pointer;
		font-size: 0.82em;
		color: var(--text-secondary);
		white-space: nowrap;
		transition: border-color 0.15s, color 0.15s;
	}

	.edit-collection-btn:hover {
		color: var(--text-primary);
		border-color: var(--text-muted);
	}
</style>
