<script lang="ts">
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { api, PadApiError, isPlanLimitError, planLimitMessage } from '$lib/api/client';
	import type { Collection, Item, QuickAction, View, ViewConfig } from '$lib/types';
	import { parseSettings, parseFields, parseSchema, getStatusOptions, itemUrlId, formatItemRef } from '$lib/types';
	import BoardView from '$lib/components/collections/BoardView.svelte';
	import ListView from '$lib/components/collections/ListView.svelte';
	import TableView from '$lib/components/collections/TableView.svelte';
	import FilterBar from '$lib/components/collections/FilterBar.svelte';
	import QuickActionsMenu from '$lib/components/common/QuickActionsMenu.svelte';
	import BottomSheet from '$lib/components/common/BottomSheet.svelte';
	import { onDestroy, onMount } from 'svelte';
	import { sseService } from '$lib/services/sse.svelte';
	import { syncService } from '$lib/services/sync.svelte';
	import { toastStore } from '$lib/stores/toast.svelte';
	import ShareDialog from '$lib/components/ShareDialog.svelte';
	import EditCollectionModal from '$lib/components/collections/EditCollectionModal.svelte';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import { titleStore } from '$lib/stores/title.svelte';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { localIndex } from '$lib/stores/localIndex.svelte';
	import { localSearch, parseSearchQuery } from '$lib/stores/localSearch.svelte';
	import { createScrollRestoration } from '$lib/scroll/restore.svelte';
	import { confirmOpenChildrenOrThrow, isOpenChildrenError } from '$lib/items/openChildrenError';

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
	let viewMode = $state<ViewMode>('list');
	let activeFilters = $state<Record<string, string>>({});
	let searchQuery = $state('');
	let showArchived = $state(false);
	let itemProgress = $state<Record<string, { total: number; done: number }>>({});
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
	let workspaceMembers = $state<{ user_id: string; role: string }[]>([]);
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
		metaLoading || (!indexReady && !indexError && !deltaSyncFailed),
	);

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

	// Sync filters to URL query params (shareable)
	function updateUrlFilters() {
		if (!collSlug || !wsSlug) return;
		const params = new URLSearchParams();
		if (viewMode !== 'list') params.set('view', viewMode);
		for (const [k, v] of Object.entries(activeFilters)) {
			params.set(k, v);
		}
		if (searchQuery) params.set('q', searchQuery);
		const qs = params.toString();
		const newUrl = `/${username}/${wsSlug}/${collSlug}${qs ? '?' + qs : ''}`;
		goto(newUrl, { replaceState: true, noScroll: true, keepFocus: true });
	}

	// Read filters from URL on load
	function loadUrlFilters() {
		const url = new URL(page.url);
		const filters: Record<string, string> = {};
		const knownParams = new Set(['view', 'q']);
		for (const [k, v] of url.searchParams.entries()) {
			if (k === 'view' && (v === 'list' || v === 'board')) {
				viewMode = v;
			} else if (k === 'q') {
				searchQuery = v;
			} else if (!knownParams.has(k)) {
				filters[k] = v;
			}
		}
		if (Object.keys(filters).length > 0) activeFilters = filters;
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
	$effect(() => {
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
				const since = localIndex.cursorFor(ws);
				const delta = await api.items.changes(ws, since);
				if (delta.changes.length === 0 || delta.cursor === since) {
					deltaSyncFailed = false;
					return true;
				}
				localIndex.applyDelta(ws, delta.changes, delta.cursor);
				if (delta.cursor === since) {
					deltaSyncFailed = false;
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
			const map: Record<string, { total: number; done: number }> = {};
			for (const p of progress) {
				map[p.item_id] = { total: p.total, done: p.done };
			}
			itemProgress = map;
		} else {
			// Non-plans collections: pull markdown-checkbox progress from
			// the new server-side endpoint. Pre-TASK-1349 this loop walked
			// `it.content` client-side, but the skinny /items-index
			// payload doesn't ship content. The server endpoint computes
			// the same counts via LENGTH/REPLACE arithmetic on the
			// stored rows — same shape `{item_id, total, done}` as
			// /plans-progress.
			//
			// Pass `includeArchived` so the toggle-on view (which renders
			// archived rows alongside live ones) still gets their
			// progress badges. Per Codex round 2 [P2] on PR #491.
			const map: Record<string, { total: number; done: number }> = {};
			const progress = await api.items
				.collectionCheckboxProgress(ws, coll, { includeArchived: showArchived })
				.catch(() => []);
			for (const p of progress) {
				map[p.item_id] = { total: p.total, done: p.done };
			}
			itemProgress = map;
		}
	}

	async function loadCollection(ws: string, coll: string, includeArchived = false) {
		metaLoading = true;
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
			collection = collData;
			savedViews = viewsData;
			workspaceMembers = membersData.members ?? [];
			activeViewId = null;

			// Fetch plan progress if viewing plans collection
			if (coll === 'plans') {
				try {
					const progress = await api.items.plansProgress(ws);
					const map: Record<string, { total: number; done: number }> = {};
					for (const p of progress) {
						map[p.item_id] = { total: p.total, done: p.done };
					}
					itemProgress = map;
					progressLabel = 'tasks';
				} catch {
					itemProgress = {};
				}
			} else {
				// Non-plans collections: pull progress from the new
				// /collections/{coll}/checkbox-progress endpoint instead
				// of parsing `item.content` client-side. /items-index
				// doesn't ship content, so the old client-side parse
				// would be a no-op; the server-side endpoint computes
				// the same counts via LENGTH/REPLACE arithmetic on the
				// stored rows. `includeArchived` is plumbed through so
				// the toggle-on view keeps progress badges on archived
				// items (per Codex round 2 [P2] on PR #491).
				try {
					const progress = await api.items.collectionCheckboxProgress(ws, coll, { includeArchived });
					const map: Record<string, { total: number; done: number }> = {};
					for (const p of progress) {
						map[p.item_id] = { total: p.total, done: p.done };
					}
					itemProgress = map;
				} catch {
					itemProgress = {};
				}
				progressLabel = 'done';
			}

			// `relationLabels` is computed reactively below — no need
			// to populate it here. (Pre-localIndex this was a one-shot
			// fetch; now plans flow into the local store and the
			// derived map below picks them up as they hydrate.)

			// Set view mode: URL param > localStorage > collection default
			const settings = parseSettings(collData);
			const defaultMode = (['board', 'list', 'table'].includes(settings.default_view))
				? settings.default_view as ViewMode : 'list';
			viewMode = loadSavedViewMode(coll, defaultMode);

			// Override with URL params if present
			loadUrlFilters();
		} catch {
			collection = null;
			// `items` is derived from localIndex; nothing to clear here.
			// A missing collection shows the empty / not-found state via
			// the `collection` null branch in the template.
		} finally {
			metaLoading = false;
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
	let hasActiveFilters = $derived(searchQuery.trim() !== '' || Object.keys(activeFilters).length > 0);

	// ── Viewport detection ───────────────────────────────────────────────
	// On mobile the 3-icon view toggle (list/board/table) is swapped for a
	// chip trigger that opens a BottomSheet with labeled options — the raw
	// icon glyphs are ambiguous on touch and a labeled sheet is clearer.
	// Desktop keeps the segmented toggle unchanged.
	let isMobile = $state(false);
	$effect(() => {
		if (typeof window === 'undefined') return;
		const mq = window.matchMedia('(max-width: 639.98px)');
		isMobile = mq.matches;
		const onChange = (e: MediaQueryListEvent) => {
			isMobile = e.matches;
			// If the viewport crosses above the mobile breakpoint while the
			// sheet is open (e.g. rotation), close it so a return to mobile
			// doesn't immediately re-mount the open sheet.
			if (!e.matches) {
				viewSheetOpen = false;
			}
		};
		mq.addEventListener('change', onChange);
		return () => mq.removeEventListener('change', onChange);
	});

	let viewSheetOpen = $state(false);
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

		const doUpdate = (force: boolean) =>
			api.items.update(ws, item.id, { fields: fieldsPayload, ...(force ? { force: true } : {}) });

		try {
			const updated = await doUpdate(false);
			// Push the canonical post-update row into the local index;
			// the `items` derived view re-renders automatically.
			localIndex.upsert(ws, updated);
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
					localIndex.upsert(ws, forced);
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
				const updated = await api.items.update(wsSlug, id, { sort_order });
				localIndex.upsert(wsSlug, updated);
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
				toastStore.show(planLimitMessage(err) + ' Upgrade to Pro at /console/billing', 'error');
			} else {
				toastStore.show(err?.message || 'Failed to create item', 'error');
			}
		} finally {
			creatingNew = false;
		}
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
			const item = await api.items.create(wsSlug, collSlug, {
				title,
				content: '',
				fields: JSON.stringify(defaultFields),
				source: 'web'
			});
			localIndex.upsert(wsSlug, item);
			quickCreateTitle = '';
			toastStore.show(`Created "${title}"`, 'success');
		} catch (err: any) {
			if (isPlanLimitError(err)) {
				toastStore.show(planLimitMessage(err) + ' Upgrade to Pro at /console/billing', 'error');
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
					goto(`/${username}/${wsSlug}/${collSlug}/${itemUrlId(item)}`);
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

	async function handleBulkArchive(itemsToArchive: Item[]) {
		if (!wsSlug) return;
		const count = itemsToArchive.length;
		try {
			for (const item of itemsToArchive) {
				await api.items.delete(wsSlug, item.id);
			}
			// Server-side delete succeeded; pull the soft-delete
			// tombstones into the local index. Gate the success
			// toast on a successful deltaSync so the user sees the
			// rows actually disappear from the view (Codex P3 round 2
			// of TASK-1357). On deltaSync failure the deletes are
			// still real server-side — SSE catches the cache up — but
			// we surface a softer "queued" message so the user knows
			// the UI hasn't reflected it yet.
			const ok = await deltaSync(wsSlug);
			if (ok) {
				toastStore.show(`Archived ${count} item${count !== 1 ? 's' : ''}`, 'success');
			} else {
				toastStore.show(
					`Archived ${count} item${count !== 1 ? 's' : ''} (updating…)`,
					'success',
				);
			}
		} catch {
			toastStore.show('Failed to archive some items', 'error');
		}
	}

	async function handleRestore(item: Item) {
		if (!wsSlug) return;
		try {
			const restored = await api.items.restore(wsSlug, item.id);
			localIndex.upsert(wsSlug, restored);
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
		const filterEntries = Object.entries(activeFilters);
		if (filterEntries.length > 0) {
			config.filters = filterEntries.map(([field, value]) => ({ field, op: 'eq', value }));
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
		if (config.filters) {
			for (const f of config.filters) {
				if (f.op === 'eq' && typeof f.value === 'string') {
					newFilters[f.field] = f.value;
				}
			}
		}
		activeFilters = newFilters;
		searchQuery = '';
		searchResultRank = null;

		// Open filters panel if the view has filters
		if (Object.keys(newFilters).length > 0) {
			filtersOpen = true;
		}

		activeViewId = view.id;
		updateUrlFilters();
	}

	function clearActiveView() {
		activeViewId = null;
		activeFilters = {};
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

<div class="collection-page" class:board-active={viewMode === 'board'}>
	{#if loading}
		<div class="loading">Loading...</div>
	{:else if !collection}
		<div class="empty-state">Collection not found</div>
	{:else}
		<!-- Header -->
		<div class="page-header">
			<div class="title-row">
				<h1>
					{#if collection.icon}<span class="collection-icon">{collection.icon}</span>{/if}
					{collection.name}
					<span class="item-count">{items.length}</span>
				</h1>

				<div class="header-actions">
					{#if isMobile}
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
						bind:searchInputEl
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
		{:else if filteredItems.length === 0 && (searchQuery || Object.keys(activeFilters).length > 0)}
			<div class="empty-state-box">
				<div class="empty-icon">🔍</div>
				<h2>No matches</h2>
				<p>No items match your current filters.
					<button class="clear-link" onclick={() => { activeFilters = {}; searchQuery = ''; searchResultRank = null; }}>Clear filters</button>
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
				onArchiveColumn={handleBulkArchive}
				onGroupReorder={handleGroupReorder}
				oncreate={canEditThisCollection ? openQuickCreate : undefined}
				{itemProgress}
				{progressLabel}
				canEdit={canEditThisCollection}
				preserveOrder={searchQuery.trim() !== ''}
			/>
		{:else if viewMode === 'table'}
			<TableView
				items={filteredItems}
				{collection}
				{wsSlug}
				onStatusChange={handleStatusChange}
				oncreate={canEditThisCollection ? openQuickCreate : undefined}
				{itemProgress}
				{progressLabel}
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
				onArchiveGroup={handleBulkArchive}
				onGroupReorder={handleGroupReorder}
				oncreate={canEditThisCollection ? openQuickCreate : undefined}
				{itemProgress}
				{progressLabel}
				canEdit={canEditThisCollection}
				preserveOrder={searchQuery.trim() !== ''}
			/>
		{/if}
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

<style>
	.collection-page {
		max-width: var(--content-max-width);
		margin: 0 auto;
		padding: var(--space-8) var(--space-6);
	}

	.collection-page.board-active {
		max-width: none;
		padding: var(--space-6) var(--space-6);
		height: 100vh;
		display: flex;
		flex-direction: column;
		overflow: hidden;
	}
	.board-active .page-header {
		flex-shrink: 0;
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
