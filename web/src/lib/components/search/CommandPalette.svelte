<script lang="ts">
	import { goto } from '$app/navigation';
	import { api } from '$lib/api/client';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { localIndex } from '$lib/stores/localIndex.svelte';
	import { localSearch, parseSearchQuery } from '$lib/stores/localSearch.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import type {
		SearchResult,
		SearchFacets,
		SearchFilters,
		Item,
		ItemIndexRow,
	} from '$lib/types';
	import { getFieldValue, itemUrlId, formatItemRef } from '$lib/types';
	import { relativeTime } from '$lib/utils/markdown';

	const RECENT_SEARCHES_KEY = 'pad-recent-searches';
	const MAX_RECENT = 10;
	const PAGE_SIZE = 20;
	// Cross-workspace results merged from N ready workspaces — cap the
	// fan-out so an extreme power user with 50 hydrated workspaces
	// doesn't pay a 50× O(matching-docs) cost per keystroke. 20 is the
	// max display anyway (PAGE_SIZE), but pulling the top 60 across
	// workspaces (20 each from top 3) gives enough headroom for the
	// merge step to keep representative coverage. PLAN-1343 / TASK-1365.
	const LOCAL_PER_WS_LIMIT = 20;

	// Augmented result — adds the source workspace so cross-workspace
	// navigation lands on the right URL. For results returned by the
	// server `/search` endpoint (single-workspace path), `workspace`
	// stays undefined and `selectResult` falls back to
	// `workspaceStore.current`. For local results, the workspace is
	// resolved from `workspaceStore.workspaces`.
	interface AugmentedSearchResult extends SearchResult {
		workspace?: { slug: string; owner_username: string };
	}

	let query = $state('');
	let results = $state<AugmentedSearchResult[]>([]);
	let total = $state(0);
	let facets = $state<SearchFacets | undefined>(undefined);
	// `searchAllWorkspaces` toggle: when on, search every workspace
	// whose `localIndex` is `'ready'` in addition to the current one.
	// Non-ready workspaces fall back to server search transparently.
	// Stored in localStorage so the toggle survives reloads.
	let searchAllWorkspaces = $state(loadAllWorkspacesPref());
	// -1 means "no result armed". The user must press an arrow key (or type a
	// bare number) before Enter will navigate. See BUG-864.
	let selectedIdx = $state(-1);
	let loading = $state(false);
	let loadingMore = $state(false);
	let searchTimeout: ReturnType<typeof setTimeout>;
	let inputEl = $state<HTMLInputElement>();

	// Filters
	let filterCollection = $state<string | null>(null);
	let filterStatus = $state<string | null>(null);

	// Recent searches
	let recentSearches = $state<string[]>(loadRecentSearches());

	// Derived: whether any filter is active
	let hasFilters = $derived(filterCollection !== null || filterStatus !== null);

	// Derived: group results by collection when not filtering by collection
	let groupedResults = $derived.by(() => {
		if (filterCollection || results.length === 0) return null;
		const groups: Record<string, { icon: string; name: string; results: SearchResult[] }> = {};
		for (const r of results) {
			const slug = r.item.collection_slug || 'unknown';
			if (!groups[slug]) {
				const coll = collectionStore.collections.find((c) => c.slug === slug);
				groups[slug] = {
					icon: r.item.collection_icon || coll?.icon || '📦',
					name: coll?.name || slug,
					results: []
				};
			}
			groups[slug].results.push(r);
		}
		return groups;
	});

	// Derived: flat index list for keyboard navigation
	let flatResults = $derived(results);

	$effect(() => {
		if (uiStore.searchOpen) {
			requestAnimationFrame(() => inputEl?.focus());
		} else {
			query = '';
			results = [];
			total = 0;
			facets = undefined;
			selectedIdx = -1;
			filterCollection = null;
			filterStatus = null;
			loading = false;
		}
	});

	/*
		Body-scroll lock while the palette is open (TASK-1124 follow-up).
		Prevents the page underneath from scrolling — both for general
		modal-correctness reasons and so the underlying page's sticky
		elements can't shift into view behind the overlay.

		Critical: only lock `overflow`, NOT `touch-action`. Setting
		`touch-action: none` on body kills scrolling on every descendant
		too — touches starting on .results would walk up the ancestor
		chain, find `none` on body, and the browser refuses to scroll
		ANY element. `overflow: hidden` alone is enough to stop body
		scroll while leaving child overflow:auto regions scrollable.
	*/
	$effect(() => {
		if (!uiStore.searchOpen) return;
		const prevOverflow = document.body.style.overflow;
		document.body.style.overflow = 'hidden';
		return () => {
			document.body.style.overflow = prevOverflow;
		};
	});

	function buildFilters(offset = 0, wsSlug?: string): SearchFilters {
		const filters: SearchFilters = {
			workspace: wsSlug ?? workspaceStore.current?.slug,
			limit: PAGE_SIZE,
			offset
		};
		if (filterCollection) filters.collection = filterCollection;
		if (filterStatus) filters.status = filterStatus;
		return filters;
	}

	/**
	 * Materialize a localSearch `{ id, score }[]` hit list into the
	 * SearchResult shape consumed by the palette template. Drops hits
	 * whose row has fallen out of `localIndex` (stale rebuild race) and
	 * tags each result with its source workspace so cross-workspace
	 * navigation works. TASK-1365.
	 */
	function materializeLocalHits(
		wsSlug: string,
		ownerUsername: string,
		hits: { id: string; score: number }[],
	): AugmentedSearchResult[] {
		const out: AugmentedSearchResult[] = [];
		for (const h of hits) {
			const row = localIndex.findByIdOrSlug(wsSlug, h.id);
			if (!row) continue;
			// `Item` widens `ItemIndexRow` by adding `content` (always ''
			// since the local index doesn't carry the rich-text body).
			// The CommandPalette only reads title/fields/ref/etc., so an
			// empty content body is fine — and matches the same widening
			// pattern the collection page uses at TASK-1357.
			const item: Item = { ...(row as ItemIndexRow), content: '' } as Item;
			out.push({
				item,
				snippet: '',
				rank: h.score,
				workspace: { slug: wsSlug, owner_username: ownerUsername },
			});
		}
		return out;
	}

	/**
	 * Identify the workspaces eligible for in-RAM local search:
	 *   - Always the current workspace if it's `'ready'`.
	 *   - When `searchAllWorkspaces` is on, every other workspace whose
	 *     `localIndex.bootstrapStateFor` is `'ready'`.
	 *
	 * Non-ready workspaces are NOT included here — they don't have a
	 * MiniSearch index built yet, and triggering bootstrap from the
	 * search palette would be surprising. They fall back to server
	 * search via the alternate path.
	 */
	function readyWorkspaces(): { slug: string; owner_username: string }[] {
		const out: { slug: string; owner_username: string }[] = [];
		const current = workspaceStore.current;
		if (current && localIndex.bootstrapStateFor(current.slug) === 'ready') {
			out.push({ slug: current.slug, owner_username: current.owner_username ?? '' });
		}
		if (!searchAllWorkspaces) return out;
		for (const ws of workspaceStore.workspaces) {
			if (current && ws.slug === current.slug) continue;
			if (localIndex.bootstrapStateFor(ws.slug) !== 'ready') continue;
			out.push({ slug: ws.slug, owner_username: ws.owner_username ?? '' });
		}
		return out;
	}

	function doSearch() {
		clearTimeout(searchTimeout);
		const trimmed = query.trim();
		if (!trimmed) {
			results = [];
			total = 0;
			facets = undefined;
			selectedIdx = -1;
			loading = false;
			return;
		}

		const parsed = parseSearchQuery(trimmed);
		const currentSlug = workspaceStore.current?.slug;
		// CRITICAL: don't read `localIndex.bootstrapStateFor` or
		// `localSearch.epoch` for body queries — those reads register
		// as reactive dependencies of the caller (`$effect`), and an
		// SSE-driven epoch bump or hydration completion mid-flight
		// would then re-fire doSearch and wipe an in-flight `loadMore`
		// append. Body queries are server-authoritative; nothing in
		// local state can change their result set. Codex round 8 P2
		// of TASK-1365.
		const currentReady = parsed.body
			? true
			: !!currentSlug && localIndex.bootstrapStateFor(currentSlug) === 'ready';
		const ready = parsed.body ? [] : readyWorkspaces();

		// Server-only paths:
		//   - `body:` / `content:` queries — local index doesn't hold
		//     the rich-text body, server FTS is the only way to grep.
		//   - Current workspace not yet hydrated — fall through to the
		//     server even if other workspaces are ready, so the user
		//     never misses results from where they are. The
		//     `searchAllWorkspaces` toggle is meant to widen, never
		//     to replace. Codex round 2 P2.
		//   - No ready workspaces at all (cold session) — fall through
		//     to the server so the palette still works pre-bootstrap.
		// All three use a 200ms debounce on the network call.
		if (parsed.body || !currentReady || ready.length === 0) {
			loading = true;
			// Snapshot the FULL dispatch scope at request time
			// (Codex rounds 4-5 P2): the response is only valid if
			// every dimension that determines what we render is still
			// the same. `isSameDispatch` must READ LIVE state for
			// readiness — capturing `currentReady` at dispatch left
			// the check stale once the index hydrated mid-request.
			const snapshotQuery = trimmed;
			const snapshotAllWs = searchAllWorkspaces;
			const snapshotBody = parsed.body;
			const snapshotFilterCollection = filterCollection;
			const snapshotFilterStatus = filterStatus;
			const snapshotWsSlug = currentSlug;
			const isSameDispatch = () => {
				if (query.trim() !== snapshotQuery) return false;
				if (searchAllWorkspaces !== snapshotAllWs) return false;
				if (filterCollection !== snapshotFilterCollection) return false;
				if (filterStatus !== snapshotFilterStatus) return false;
				if (workspaceStore.current?.slug !== snapshotWsSlug) return false;
				// Body queries grep content the local index doesn't
				// hold, so they're authoritative regardless of
				// hydration state — only the dimensions checked above
				// can invalidate them.
				if (snapshotBody) return true;
				// Non-body server fetches were triggered because the
				// current workspace wasn't ready. If it's NOW ready,
				// the local path is the authoritative source and the
				// server response is stale. Read live state.
				const liveCurrentReady =
					!!snapshotWsSlug &&
					localIndex.bootstrapStateFor(snapshotWsSlug) === 'ready';
				if (liveCurrentReady) return false;
				return true;
			};
			searchTimeout = setTimeout(async () => {
				try {
					// Bare `body:` / `content:` with no following text:
					// `parsed.text` is empty and we'd otherwise fall back
					// to the raw query (`body:`) and ship that literal
					// token to the server. Treat as a no-op until the
					// user keeps typing. Codex round 9 P3 of TASK-1365.
					if (parsed.body && !parsed.text) {
						if (isSameDispatch()) {
							results = [];
							total = 0;
							facets = undefined;
						}
						return;
					}
					const serverQuery = parsed.body ? parsed.text : trimmed;
					if (!serverQuery.trim()) {
						if (isSameDispatch()) {
							results = [];
							total = 0;
							facets = undefined;
						}
						return;
					}
					const resp = await api.search(serverQuery, buildFilters(0));
					if (!isSameDispatch()) return;
					results = resp.results ?? [];
					total = resp.total ?? 0;
					facets = resp.facets;
					selectedIdx = -1;
				} catch {
					if (isSameDispatch()) {
						results = [];
						total = 0;
						facets = undefined;
					}
				} finally {
					if (isSameDispatch()) loading = false;
				}
			}, 200);
			return;
		}

		// Local synchronous path. For each ready workspace, run
		// localSearch.search and materialize to SearchResult shape.
		// Merge by score descending — ties broken by `updated_at DESC`
		// for stability.
		//
		// Per-workspace limit: when a status filter chip is active, we
		// expand the per-workspace pull so the post-fetch
		// `filterStatus` filter has enough headroom to find matches
		// outside the top 20 (Codex round 3 P2). The status filter
		// isn't an index-aware operation on the local path — it walks
		// the parsed `fields` blob after materialization — so a tight
		// per-ws cap could drop valid lower-ranked matches.
		const perWsLimit = filterStatus
			? LOCAL_PER_WS_LIMIT * 5
			: LOCAL_PER_WS_LIMIT;
		const merged: AugmentedSearchResult[] = [];
		for (const ws of ready) {
			const hits = localSearch.search(ws.slug, trimmed, {
				collection: filterCollection ?? undefined,
				limit: perWsLimit,
			});
			merged.push(...materializeLocalHits(ws.slug, ws.owner_username, hits));
		}
		merged.sort((a, b) => {
			if (b.rank !== a.rank) return b.rank - a.rank;
			// Stable tie-break by updated_at DESC then id ASC.
			const aU = a.item.updated_at ?? '';
			const bU = b.item.updated_at ?? '';
			if (aU !== bU) return aU < bU ? 1 : -1;
			return a.item.id < b.item.id ? -1 : 1;
		});

		// Apply the status filter chip if active. (Collection filter is
		// passed through to localSearch.search above.)
		const filtered = filterStatus
			? merged.filter((r) => getFieldValue(r.item, 'status') === filterStatus)
			: merged;

		// Truncate the rendered list to PAGE_SIZE * 2 so cross-workspace
		// power users still see a representative top slice. CRITICAL:
		// set `total = results.length` so the "Load more" affordance
		// stays hidden — loadMore hits the server, which would
		// inject scope-mismatched (and duplicate) rows into the local
		// result set. Codex round 1 P2. Local results are all in RAM;
		// if more than `PAGE_SIZE * 2` match, the right answer is a
		// more specific query, not a paginated fetch.
		results = filtered.slice(0, PAGE_SIZE * 2);
		total = results.length;
		// Facets are server-only; clear them on the local path so the
		// chip row hides cleanly.
		facets = undefined;
		selectedIdx = -1;
		loading = false;
	}

	// Re-run the search whenever any tracked dependency changes:
	//   - `query` typed by the user (keystroke or recent-search click)
	//   - `searchAllWorkspaces` toggle
	//   - Any ready workspace's localSearch epoch (SSE-driven upserts /
	//     removes) — without this, an open-palette user wouldn't see
	//     freshly-created items even though the underlying index
	//     updated. PLAN-1343 / TASK-1365.
	//
	// EXCEPTION: body queries don't depend on local index state. They
	// hit the server, which is the only source of truth for content
	// text. Skip the readiness / epoch reads for body queries so a
	// concurrent SSE bump (or hydration completion) can't re-fire
	// doSearch from offset 0 and wipe an in-flight `loadMore` append.
	// Codex round 7 P2 of TASK-1365.
	//
	// `doSearch` handles the empty-query case internally by clearing
	// results — so the effect can fire on every transition (including
	// "user backspaced to empty") without leaving stale results visible.
	$effect(() => {
		if (!uiStore.searchOpen) return;
		void query;
		void searchAllWorkspaces;
		void filterCollection;
		void filterStatus;
		const trimmed = query.trim();
		const parsed = trimmed ? parseSearchQuery(trimmed) : null;
		if (!parsed?.body) {
			void localIndex.bootstrapStateFor(workspaceStore.current?.slug ?? '');
			for (const ws of workspaceStore.workspaces) {
				void localSearch.epoch(ws.slug);
			}
		}
		doSearch();
	});

	function loadAllWorkspacesPref(): boolean {
		try {
			return localStorage.getItem('pad-search-all-workspaces') === 'true';
		} catch {
			return false;
		}
	}

	function toggleAllWorkspaces() {
		searchAllWorkspaces = !searchAllWorkspaces;
		try {
			localStorage.setItem(
				'pad-search-all-workspaces',
				searchAllWorkspaces ? 'true' : 'false',
			);
		} catch {
			// Storage unavailable (private mode, quota) — silently
			// degrade; the toggle still works for the session.
		}
		// The reactive effect picks up the flip; no explicit re-run.
	}

	async function loadMore() {
		if (loadingMore || results.length >= total) return;
		// Only the server path ever sets `total > results.length` — the
		// local path pins them equal — so a `loadMore` always means
		// fetching another server page. Snapshot the dispatch scope
		// and bail if any of it has shifted before the response lands.
		// Body queries are always server-authoritative (the local
		// index doesn't carry content), so they bypass the
		// live-readiness guard. Codex rounds 5-6 P2.
		loadingMore = true;
		const snapshotQuery = query;
		const snapshotAllWs = searchAllWorkspaces;
		const snapshotCollection = filterCollection;
		const snapshotStatus = filterStatus;
		const snapshotWsSlug = workspaceStore.current?.slug;
		// Parse once and use the stripped query for the API call so
		// `body:foo` → page 2 sends `foo`, not the literal `body:foo`.
		// A bare `body:` / `content:` with no following text is a
		// no-op — there's nothing to load a second page of.
		const parsed = parseSearchQuery(query.trim());
		if (parsed.body && !parsed.text) {
			loadingMore = false;
			return;
		}
		const serverQuery = parsed.body ? parsed.text : query;
		try {
			const resp = await api.search(serverQuery, buildFilters(results.length));
			if (
				query !== snapshotQuery ||
				searchAllWorkspaces !== snapshotAllWs ||
				filterCollection !== snapshotCollection ||
				filterStatus !== snapshotStatus ||
				workspaceStore.current?.slug !== snapshotWsSlug
			) {
				return;
			}
			if (!parsed.body) {
				// Non-body server paging: if the current workspace
				// hydrated while the request was in flight, the main
				// effect has likely already swapped to the local
				// path and replaced `results`. Appending more server
				// rows would inject scope-mismatched results.
				const liveCurrentReady =
					!!snapshotWsSlug &&
					localIndex.bootstrapStateFor(snapshotWsSlug) === 'ready';
				if (liveCurrentReady) return;
			}
			results = [...results, ...(resp.results ?? [])];
		} catch {
			// ignore
		} finally {
			loadingMore = false;
		}
	}

	function applyFilter(type: 'collection' | 'status', value: string) {
		// The reactive search effect re-runs on filterCollection /
		// filterStatus changes, so no explicit doSearch() needed.
		if (type === 'collection') {
			filterCollection = filterCollection === value ? null : value;
		} else {
			filterStatus = filterStatus === value ? null : value;
		}
	}

	function clearFilters() {
		filterCollection = null;
		filterStatus = null;
	}

	function scrollSelectedIntoView() {
		requestAnimationFrame(() => {
			const el = document.querySelector('.result.selected');
			el?.scrollIntoView({ block: 'nearest' });
		});
	}

	async function handleKeydown(e: KeyboardEvent) {
		if (e.key === 'Escape') {
			uiStore.closeSearch();
		} else if (e.key === 'ArrowDown') {
			e.preventDefault();
			// From -1 ("nothing armed") this lands on 0, the first result.
			selectedIdx = Math.min(selectedIdx + 1, flatResults.length - 1);
			scrollSelectedIntoView();
		} else if (e.key === 'ArrowUp') {
			e.preventDefault();
			// Clamp at 0 — once the user has armed a selection, ArrowUp
			// shouldn't deselect back to -1.
			selectedIdx = Math.max(selectedIdx - 1, 0);
			scrollSelectedIntoView();
		} else if (e.key === 'Enter') {
			e.preventDefault();
			// Numeric go-to mode: typing a bare number + Enter jumps directly
			// to the item with that item_number — the search palette doubles
			// as a quick "go to item N" jump. See BUG-910 + BUG-864.
			const trimmed = query.trim();
			if (/^\d+$/.test(trimmed)) {
				const targetNum = parseInt(trimmed, 10);
				// Flush any pending debounced search so Enter feels instant
				// even if the user beats the 200ms debounce.
				clearTimeout(searchTimeout);
				let item = results.find((r) => r.item.item_number === targetNum);
				if (!item) {
					loading = true;
					try {
						const resp = await api.search(trimmed, buildFilters(0));
						results = resp.results ?? [];
						total = resp.total ?? 0;
						facets = resp.facets;
						item = results.find((r) => r.item.item_number === targetNum);
					} catch {
						// ignore — falls through to no-op below
					} finally {
						loading = false;
					}
				}
				if (item) {
					selectResult(item);
				}
				return;
			}
			// BUG-864: for non-numeric queries, only navigate when the user
			// has explicitly arrow-selected a result.
			if (selectedIdx >= 0 && flatResults.length > 0 && flatResults[selectedIdx]) {
				selectResult(flatResults[selectedIdx]);
			}
		}
	}

	function selectResult(r: AugmentedSearchResult) {
		saveRecentSearch(query.trim());
		// Local (cross-workspace) hits ship their source workspace on
		// the result. Server hits don't — they implicitly came from the
		// current workspace because `buildFilters` scopes the request.
		// Resolve via fallback so both paths work. TASK-1365.
		const ws = r.workspace?.slug ?? workspaceStore.current?.slug;
		const wsUsername =
			r.workspace?.owner_username ?? workspaceStore.current?.owner_username;
		const collSlug = r.item.collection_slug;
		if (ws && wsUsername && collSlug) {
			goto(`/${wsUsername}/${ws}/${collSlug}/${itemUrlId(r.item)}`);
		}
		uiStore.closeSearch();
	}

	function useRecentSearch(q: string) {
		// Assignment alone re-triggers the reactive search effect; the
		// explicit doSearch() call is no longer needed.
		query = q;
		requestAnimationFrame(() => inputEl?.focus());
	}

	function stripHtml(s: string): string {
		return s.replace(/<[^>]*>/g, '');
	}

	function statusColor(status: string): string {
		const s = status?.toLowerCase().replace(/-/g, '_');
		if (['done', 'completed', 'fixed', 'implemented', 'resolved'].includes(s))
			return 'var(--accent-green)';
		if (['in_progress', 'exploring', 'fixing'].includes(s)) return 'var(--accent-amber)';
		if (['open', 'new', 'draft', 'todo', 'planned'].includes(s)) return 'var(--accent-blue)';
		if (s === 'active') return 'var(--accent-cyan)';
		return 'var(--text-muted)';
	}

	function priorityDot(priority: string): string {
		const p = priority?.toLowerCase();
		if (p === 'critical') return '\u{1F534}';
		if (p === 'high') return '\u{1F7E0}';
		if (p === 'medium') return '\u{1F7E1}';
		if (p === 'low') return '\u26AA';
		return '';
	}

	// Recent searches persistence
	function loadRecentSearches(): string[] {
		try {
			const raw = localStorage.getItem(RECENT_SEARCHES_KEY);
			return raw ? JSON.parse(raw) : [];
		} catch {
			return [];
		}
	}

	function saveRecentSearch(q: string) {
		if (!q) return;
		const filtered = recentSearches.filter((s) => s !== q);
		recentSearches = [q, ...filtered].slice(0, MAX_RECENT);
		try {
			localStorage.setItem(RECENT_SEARCHES_KEY, JSON.stringify(recentSearches));
		} catch {
			// ignore
		}
	}

	function clearRecentSearches() {
		recentSearches = [];
		try {
			localStorage.removeItem(RECENT_SEARCHES_KEY);
		} catch {
			// ignore
		}
	}

	function renderResultCard(r: AugmentedSearchResult, i: number): { ref: string | null; status: string | undefined; priority: string | undefined } {
		void i;
		return {
			ref: formatItemRef(r.item),
			status: getFieldValue(r.item, 'status'),
			priority: getFieldValue(r.item, 'priority')
		};
	}

	// Derived: ready workspaces other than the current one. Used to gate
	// the "search all workspaces" toggle visibility — no point showing
	// it when the user only has the current workspace hydrated.
	let otherReadyWorkspaceCount = $derived.by(() => {
		const current = workspaceStore.current?.slug;
		let n = 0;
		for (const ws of workspaceStore.workspaces) {
			if (ws.slug === current) continue;
			if (localIndex.bootstrapStateFor(ws.slug) === 'ready') n += 1;
		}
		return n;
	});
</script>

{#if uiStore.searchOpen}
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<!-- svelte-ignore a11y_no_static_element_interactions -->
	<div class="overlay" onclick={() => uiStore.closeSearch()}>
		<div class="palette" onclick={(e) => e.stopPropagation()} onkeydown={handleKeydown}>
			<!-- Search input -->
			<div class="search-row">
				<svg
					class="search-icon"
					width="16"
					height="16"
					viewBox="0 0 24 24"
					fill="none"
					stroke="currentColor"
					stroke-width="2"
					><circle cx="11" cy="11" r="8" /><line
						x1="21"
						y1="21"
						x2="16.65"
						y2="16.65"
					/></svg
				>
				<input
					bind:this={inputEl}
					bind:value={query}
					placeholder="Search items, collections, docs..."
					class="search-input"
				/>
				{#if loading}
					<span class="search-spinner"></span>
				{/if}
				<kbd class="search-hint">esc</kbd>
				<!--
					Mobile-only close button (TASK-1124). On mobile the palette
					is a full-screen takeover so there's no overlay area to
					tap-to-dismiss, and the `esc` hint is useless without a
					keyboard. CSS hides this on desktop and hides .search-hint
					on mobile so each surface gets the right affordance.
				-->
				<button
					class="mobile-close"
					onclick={() => uiStore.closeSearch()}
					aria-label="Close search"
					title="Close"
				>
					<svg width="20" height="20" viewBox="0 0 20 20" fill="none" aria-hidden="true">
						<path d="M5 5L15 15M15 5L5 15" stroke="currentColor" stroke-width="1.75" stroke-linecap="round"/>
					</svg>
				</button>
			</div>

			<!--
				"All workspaces" toggle (TASK-1365). Only surfaced when the
				user has more than one ready workspace — if you only have
				the current workspace hydrated, the toggle would be a no-op
				and just adds chrome.
			-->
			{#if otherReadyWorkspaceCount > 0}
				<div class="scope-row">
					<button
						class="scope-toggle"
						class:active={searchAllWorkspaces}
						onclick={toggleAllWorkspaces}
						title="When on, search every workspace that's already loaded this session"
					>
						<span class="scope-dot" class:on={searchAllWorkspaces}></span>
						<span>All workspaces ({otherReadyWorkspaceCount + 1} ready)</span>
					</button>
				</div>
			{/if}

			<!--
				Filter chips. Surface in three cases:
				  1. Server search returned facets (full chip vocabulary).
				  2. Local path with an active filter — show the active
				     chip(s) alone so the user can see + clear them.
				     Without this branch, switching from server → local
				     with a filter chip selected would hide the chip but
				     keep filtering, leaving the user no clear way to
				     remove it. Codex round 2 P2 of TASK-1365.
			-->
			{#if facets && query.trim()}
				<div class="filters-row">
					<div class="filter-chips">
						{#if facets.collections && Object.keys(facets.collections).length > 0}
							{#each Object.entries(facets.collections) as [slug, count] (slug)}
								{@const coll = collectionStore.collections.find((c) => c.slug === slug)}
								<button
									class="filter-chip"
									class:active={filterCollection === slug}
									onclick={() => applyFilter('collection', slug)}
								>
									<span class="chip-icon">{coll?.icon || '📦'}</span>
									<span class="chip-label">{coll?.name || slug}</span>
									<span class="chip-count">{count}</span>
								</button>
							{/each}
						{/if}
						{#if facets.statuses && Object.keys(facets.statuses).length > 0}
							{#each Object.entries(facets.statuses) as [status, count] (status)}
								<button
									class="filter-chip status-chip"
									class:active={filterStatus === status}
									onclick={() => applyFilter('status', status)}
								>
									<span
										class="chip-dot"
										style="background: {statusColor(status)};"
									></span>
									<span class="chip-label">{status.replace(/_/g, ' ')}</span>
									<span class="chip-count">{count}</span>
								</button>
							{/each}
						{/if}
					</div>
					{#if hasFilters}
						<button class="clear-filters" onclick={clearFilters}>Clear filters</button>
					{/if}
				</div>
			{:else if hasFilters && query.trim()}
				<div class="filters-row">
					<div class="filter-chips">
						{#if filterCollection}
							{@const coll = collectionStore.collections.find((c) => c.slug === filterCollection)}
							<button
								class="filter-chip active"
								onclick={() => applyFilter('collection', filterCollection!)}
							>
								<span class="chip-icon">{coll?.icon || '📦'}</span>
								<span class="chip-label">{coll?.name || filterCollection}</span>
							</button>
						{/if}
						{#if filterStatus}
							<button
								class="filter-chip status-chip active"
								onclick={() => applyFilter('status', filterStatus!)}
							>
								<span
									class="chip-dot"
									style="background: {statusColor(filterStatus)};"
								></span>
								<span class="chip-label">{filterStatus.replace(/_/g, ' ')}</span>
							</button>
						{/if}
					</div>
					<button class="clear-filters" onclick={clearFilters}>Clear filters</button>
				</div>
			{/if}

			<!-- Result count -->
			{#if results.length > 0 && query.trim()}
				<div class="result-count">
					{total} result{total === 1 ? '' : 's'}
				</div>
			{/if}

			<!-- Results -->
			{#if results.length > 0}
				<div class="results">
					{#if groupedResults && !filterCollection}
						<!-- Grouped by collection -->
						{#each Object.entries(groupedResults) as [slug, group] (slug)}
							<div class="result-group">
								<div class="group-header">
									<span class="group-icon">{group.icon}</span>
									<span class="group-name">{group.name}</span>
									<span class="group-count">{group.results.length}</span>
								</div>
								{#each group.results as r (r.item.id || r.item.slug)}
									{@const idx = flatResults.indexOf(r)}
									{@const meta = renderResultCard(r, idx)}
									<button
										class="result"
										class:selected={idx === selectedIdx}
										onclick={() => selectResult(r)}
									>
										<div class="result-main">
											<span class="result-icon"
												>{r.item.collection_icon || '📦'}</span
											>
											{#if meta.ref}
												<span class="result-ref">{meta.ref}</span>
											{/if}
											<span class="result-title">{r.item.title}</span>
											{#if meta.priority}
												{@const dot = priorityDot(meta.priority)}
												{#if dot}
													<span class="result-priority" title={meta.priority}
														>{dot}</span
													>
												{/if}
											{/if}
											{#if meta.status}
												<span
													class="result-status"
													style="background: color-mix(in srgb, {statusColor(meta.status)} 15%, transparent); color: {statusColor(meta.status)};"
												>
													{meta.status.replace(/_/g, ' ')}
												</span>
											{/if}
											{#if r.item.updated_at}
												<span class="result-date"
													>{relativeTime(r.item.updated_at)}</span
												>
											{/if}
										</div>
										{#if r.snippet}
											<div class="result-snippet">
												{stripHtml(r.snippet)}
											</div>
										{/if}
									</button>
								{/each}
							</div>
						{/each}
					{:else}
						<!-- Flat list (filtered by collection or no grouping) -->
						{#each results as r, i (r.item.id || r.item.slug)}
							{@const meta = renderResultCard(r, i)}
							<button
								class="result"
								class:selected={i === selectedIdx}
								onclick={() => selectResult(r)}
							>
								<div class="result-main">
									<span class="result-icon"
										>{r.item.collection_icon || '📦'}</span
									>
									{#if meta.ref}
										<span class="result-ref">{meta.ref}</span>
									{/if}
									<span class="result-title">{r.item.title}</span>
									{#if meta.priority}
										{@const dot = priorityDot(meta.priority)}
										{#if dot}
											<span class="result-priority" title={meta.priority}
												>{dot}</span
											>
										{/if}
									{/if}
									{#if meta.status}
										<span
											class="result-status"
											style="background: color-mix(in srgb, {statusColor(meta.status)} 15%, transparent); color: {statusColor(meta.status)};"
										>
											{meta.status.replace(/_/g, ' ')}
										</span>
									{/if}
									{#if r.item.updated_at}
										<span class="result-date"
											>{relativeTime(r.item.updated_at)}</span
										>
									{/if}
								</div>
								{#if r.snippet}
									<div class="result-snippet">{stripHtml(r.snippet)}</div>
								{/if}
							</button>
						{/each}
					{/if}

					<!-- Load more -->
					{#if results.length < total}
						<div class="load-more-row">
							<button class="load-more-btn" onclick={loadMore} disabled={loadingMore}>
								{loadingMore ? 'Loading...' : `Load more (${total - results.length} remaining)`}
							</button>
						</div>
					{/if}
				</div>
			{:else if query.trim() && !loading}
				<div class="no-results">No results for "{query}"</div>
			{:else if !query.trim()}
				<!-- Recent searches or tips -->
				{#if recentSearches.length > 0}
					<div class="recent-searches">
						<div class="recent-header">
							<span class="recent-label">Recent searches</span>
							<button class="clear-recent" onclick={clearRecentSearches}
								>Clear recent</button
							>
						</div>
						{#each recentSearches as recent, i (recent)}
							<button
								class="recent-item"
								class:selected={i === selectedIdx}
								onclick={() => useRecentSearch(recent)}
							>
								<svg
									width="14"
									height="14"
									viewBox="0 0 24 24"
									fill="none"
									stroke="currentColor"
									stroke-width="2"
									class="recent-icon"
								>
									<polyline points="1 4 1 10 7 10" />
									<path d="M3.51 15a9 9 0 1 0 2.13-9.36L1 10" />
								</svg>
								<span class="recent-text">{recent}</span>
							</button>
						{/each}
					</div>
				{:else}
					<div class="search-tips">
						<span class="tip-label">Try searching for</span>
						<span class="tip-example">task names, ideas, docs, or any text</span>
					</div>
				{/if}
			{/if}
		</div>
	</div>
{/if}

<style>
	.overlay {
		position: fixed;
		inset: 0;
		background: rgba(0, 0, 0, 0.5);
		z-index: 50;
		display: flex;
		justify-content: center;
		padding-top: 12vh;
	}
	.palette {
		width: 100%;
		max-width: 640px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		box-shadow: 0 20px 60px rgba(0, 0, 0, 0.5);
		overflow: hidden;
		max-height: 65vh;
		display: flex;
		flex-direction: column;
	}
	.search-row {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: 0 var(--space-4);
		border-bottom: 1px solid var(--border);
		flex-shrink: 0;
	}
	.search-icon {
		color: var(--text-muted);
		flex-shrink: 0;
	}
	.search-input {
		flex: 1;
		padding: var(--space-4) 0;
		background: transparent;
		border: none;
		font-size: 1.1em;
		border-radius: 0;
	}
	.search-input:focus {
		border: none;
	}
	.search-spinner {
		width: 16px;
		height: 16px;
		border: 2px solid var(--border);
		border-top-color: var(--text-muted);
		border-radius: 50%;
		animation: spin 0.6s linear infinite;
		flex-shrink: 0;
	}
	@keyframes spin {
		to {
			transform: rotate(360deg);
		}
	}
	.search-hint {
		font-size: 0.7em;
		color: var(--text-muted);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		padding: 1px 6px;
		border-radius: 3px;
		font-family: var(--font-mono);
		flex-shrink: 0;
	}

	/* "All workspaces" scope toggle (TASK-1365) */
	.scope-row {
		display: flex;
		align-items: center;
		padding: var(--space-1) var(--space-3);
		border-bottom: 1px solid var(--border);
		flex-shrink: 0;
	}
	.scope-toggle {
		display: inline-flex;
		align-items: center;
		gap: var(--space-2);
		padding: 2px 8px;
		border-radius: var(--radius);
		font-size: 0.75em;
		color: var(--text-muted);
		background: none;
		border: 1px solid transparent;
		cursor: pointer;
		transition: all 0.15s ease;
	}
	.scope-toggle:hover {
		background: var(--bg-hover);
		color: var(--text-secondary);
	}
	.scope-toggle.active {
		color: var(--accent-blue);
		border-color: color-mix(in srgb, var(--accent-blue) 30%, transparent);
		background: color-mix(in srgb, var(--accent-blue) 10%, transparent);
	}
	.scope-dot {
		width: 8px;
		height: 8px;
		border-radius: 50%;
		background: var(--text-muted);
		border: 1px solid var(--border);
	}
	.scope-dot.on {
		background: var(--accent-blue);
		border-color: var(--accent-blue);
	}

	/* Filter chips */
	.filters-row {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-2) var(--space-3);
		border-bottom: 1px solid var(--border);
		flex-shrink: 0;
		flex-wrap: wrap;
	}
	.filter-chips {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-1);
		flex: 1;
	}
	.filter-chip {
		display: inline-flex;
		align-items: center;
		gap: 4px;
		padding: 2px 8px;
		border-radius: 999px;
		font-size: 0.75em;
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		color: var(--text-secondary);
		cursor: pointer;
		transition: all 0.15s ease;
	}
	.filter-chip:hover {
		background: var(--bg-hover);
	}
	.filter-chip.active {
		background: color-mix(in srgb, var(--accent-blue) 20%, transparent);
		border-color: var(--accent-blue);
		color: var(--accent-blue);
	}
	.chip-icon {
		font-size: 1em;
	}
	.chip-label {
		text-transform: capitalize;
	}
	.chip-count {
		color: var(--text-muted);
		font-size: 0.9em;
	}
	.chip-dot {
		width: 6px;
		height: 6px;
		border-radius: 50%;
		flex-shrink: 0;
	}
	.clear-filters {
		font-size: 0.72em;
		color: var(--text-muted);
		background: none;
		border: none;
		cursor: pointer;
		padding: 2px 6px;
		border-radius: var(--radius);
		flex-shrink: 0;
	}
	.clear-filters:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
	}

	/* Result count */
	.result-count {
		padding: var(--space-1) var(--space-4);
		font-size: 0.75em;
		color: var(--text-muted);
		border-bottom: 1px solid var(--border);
		flex-shrink: 0;
	}

	/* Results */
	.results {
		overflow-y: auto;
		padding: var(--space-2);
	}

	/* Group headers */
	.result-group {
		margin-bottom: var(--space-2);
	}
	.group-header {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-2) var(--space-3);
		font-size: 0.75em;
		color: var(--text-muted);
		text-transform: uppercase;
		letter-spacing: 0.04em;
		font-weight: 600;
	}
	.group-icon {
		font-size: 1.1em;
	}
	.group-name {
		flex: 1;
	}
	.group-count {
		font-weight: 400;
	}

	/* Result cards */
	.result {
		display: block;
		width: 100%;
		text-align: left;
		padding: var(--space-2) var(--space-3);
		border-radius: var(--radius);
	}
	.result:hover,
	.result.selected {
		background: var(--bg-hover);
	}
	.result-main {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}
	.result-icon {
		font-size: 1em;
		flex-shrink: 0;
	}
	.result-ref {
		font-family: var(--font-mono);
		font-size: 0.75em;
		color: var(--text-muted);
		flex-shrink: 0;
	}
	.result-title {
		font-weight: 500;
		flex: 1;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.result-priority {
		font-size: 0.8em;
		flex-shrink: 0;
		line-height: 1;
	}
	.result-status {
		font-size: 0.7em;
		padding: 2px 8px;
		border-radius: 999px;
		flex-shrink: 0;
		text-transform: capitalize;
	}
	.result-date {
		font-size: 0.7em;
		color: var(--text-muted);
		flex-shrink: 0;
		white-space: nowrap;
	}
	.result-snippet {
		font-size: 0.85em;
		color: var(--text-muted);
		margin-top: 2px;
		margin-left: 24px;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	/* Load more */
	.load-more-row {
		padding: var(--space-2) var(--space-3);
		text-align: center;
	}
	.load-more-btn {
		font-size: 0.8em;
		color: var(--text-secondary);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		padding: var(--space-2) var(--space-4);
		border-radius: var(--radius);
		cursor: pointer;
		width: 100%;
	}
	.load-more-btn:hover:not(:disabled) {
		background: var(--bg-hover);
	}
	.load-more-btn:disabled {
		opacity: 0.6;
		cursor: not-allowed;
	}

	/* No results / tips */
	.no-results {
		padding: var(--space-4);
		text-align: center;
		color: var(--text-muted);
	}
	.search-tips {
		padding: var(--space-4);
		text-align: center;
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}
	.tip-label {
		font-size: 0.8em;
		color: var(--text-muted);
	}
	.tip-example {
		font-size: 0.85em;
		color: var(--text-secondary);
	}

	/* Recent searches */
	.recent-searches {
		padding: var(--space-2);
	}
	.recent-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-1) var(--space-3);
		margin-bottom: var(--space-1);
	}
	.recent-label {
		font-size: 0.75em;
		color: var(--text-muted);
		text-transform: uppercase;
		letter-spacing: 0.04em;
		font-weight: 600;
	}
	.clear-recent {
		font-size: 0.72em;
		color: var(--text-muted);
		background: none;
		border: none;
		cursor: pointer;
		padding: 2px 6px;
		border-radius: var(--radius);
	}
	.clear-recent:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
	}
	.recent-item {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		width: 100%;
		text-align: left;
		padding: var(--space-2) var(--space-3);
		border-radius: var(--radius);
		font-size: 0.9em;
		color: var(--text-secondary);
	}
	.recent-item:hover,
	.recent-item.selected {
		background: var(--bg-hover);
	}
	.recent-icon {
		color: var(--text-muted);
		flex-shrink: 0;
	}
	.recent-text {
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	/*
		Mobile-only close button (TASK-1124). Hidden on desktop where the
		`esc` keyboard hint suffices; visible on mobile where there's no
		keyboard and the full-screen palette has no overlay area to dismiss
		via tap-outside. Sized to match the .mobile-hamburger / icon-button
		vocabulary used elsewhere in the mobile chrome.
	*/
	.mobile-close {
		display: none;
		align-items: center;
		justify-content: center;
		width: 32px;
		height: 32px;
		border-radius: var(--radius-sm);
		color: var(--text-secondary);
		background: none;
		border: none;
		cursor: pointer;
		padding: 0;
		flex-shrink: 0;
		transition: color 0.15s, background 0.15s;
	}
	.mobile-close:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
	}

	/*
		Mobile UX overrides for the CommandPalette (IDEA-1121 / TASK-1124).
		Desktop is byte-identical above this rule — every property here is
		scoped to ≤768px (the same breakpoint uiStore.isMobile uses, so JS
		layout decisions and CSS layout stay in sync).

		Key changes:

		• Full-screen takeover. The palette fills the viewport (no rounded
		  corners, no shadow, no max-width / max-height) so the search input
		  anchors at the very top. With the input at the top, the on-screen
		  keyboard pops up from the bottom and the input never has to scroll
		  to stay visible — eliminating the "things shift when keyboard
		  appears" problem the centered desktop layout has on phones.

		• 100dvh (dynamic viewport height) instead of 100vh. On iOS Safari
		  15+ and modern Chrome, `dvh` shrinks when the on-screen keyboard
		  is shown, so the palette is sized to the *visible* viewport above
		  the keyboard rather than the full screen behind it. This is what
		  actually makes the layout stop fighting the keyboard. Older
		  browsers fall back to the standard viewport (treats as vh).

		• 16px input font. Below 16px iOS Safari auto-zooms the page on
		  focus, which is the root cause of a separate "everything jumps"
		  jitter when tapping the input. Forcing 16px on mobile is the
		  standard mobile-web fix.

		• Hide the `esc` kbd hint, show the X close button. The hint is
		  meaningless without a hardware keyboard; the X gives users a clear
		  tap target since the full-screen palette leaves no overlay to
		  tap-outside.
	*/
	@media (max-width: 768px) {
		.overlay {
			padding-top: 0;
			/*
				Belt-and-braces with the JS body-scroll lock: even if iOS
				somehow drags the body, the overlay itself can't scroll its
				own contents. The palette below is height: 100dvh so it
				exactly fills the overlay; nothing should ever overflow.
			*/
			overflow: hidden;
			overscroll-behavior: contain;
		}
		.palette {
			max-width: none;
			max-height: 100dvh;
			height: 100dvh;
			width: 100%;
			border: none;
			border-radius: 0;
			box-shadow: none;
		}
		.search-input {
			/* iOS Safari skips its focus-zoom only when font-size ≥ 16px. */
			font-size: 16px;
		}
		.search-hint {
			display: none;
		}
		.mobile-close {
			display: flex;
		}
		/*
			Pin .results as the SOLE scroll target inside the palette so
			the search-row stays at the top regardless of result-list
			length. `flex: 1; min-height: 0` lets the flex item shrink
			below its content size (without min-height: 0, an item with
			overflow: auto in a flex column refuses to shrink and the
			whole palette ends up taller than 100dvh — which is what made
			the input scroll off-screen in the first reported bug).
			`overscroll-behavior: contain` prevents bounce-scroll from
			chaining out to the body, which on iOS would re-trigger the
			scroll-to-input quirk we're trying to suppress.
		*/
		.results {
			flex: 1;
			min-height: 0;
			overscroll-behavior: contain;
			-webkit-overflow-scrolling: touch;
		}
	}
</style>
