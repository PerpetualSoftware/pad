<script lang="ts">
	import { goto } from '$app/navigation';
	import { api } from '$lib/api/client';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import type { SearchResult, SearchFacets, SearchFilters } from '$lib/types';
	import { getFieldValue, itemUrlId, formatItemRef } from '$lib/types';
	import { relativeTime } from '$lib/utils/markdown';

	const RECENT_SEARCHES_KEY = 'pad-recent-searches';
	const MAX_RECENT = 10;
	const PAGE_SIZE = 20;

	let query = $state('');
	let results = $state<SearchResult[]>([]);
	let total = $state(0);
	let facets = $state<SearchFacets | undefined>(undefined);
	let selectedIdx = $state(0);
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
			selectedIdx = 0;
			filterCollection = null;
			filterStatus = null;
			loading = false;
		}
	});

	function buildFilters(offset = 0): SearchFilters {
		const filters: SearchFilters = {
			workspace: workspaceStore.current?.slug,
			limit: PAGE_SIZE,
			offset
		};
		if (filterCollection) filters.collection = filterCollection;
		if (filterStatus) filters.status = filterStatus;
		return filters;
	}

	function doSearch() {
		clearTimeout(searchTimeout);
		if (!query.trim()) {
			results = [];
			total = 0;
			facets = undefined;
			selectedIdx = 0;
			loading = false;
			return;
		}
		loading = true;
		searchTimeout = setTimeout(async () => {
			try {
				const resp = await api.search(query, buildFilters(0));
				// Defensive: some backends / error paths can send `null` for
				// an absent array. Coalesce so downstream `.length` is safe.
				results = resp.results ?? [];
				total = resp.total ?? 0;
				facets = resp.facets;
				selectedIdx = 0;
			} catch {
				results = [];
				total = 0;
				facets = undefined;
			} finally {
				loading = false;
			}
		}, 200);
	}

	async function loadMore() {
		if (loadingMore || results.length >= total) return;
		loadingMore = true;
		const snapshotQuery = query;
		const snapshotCollection = filterCollection;
		const snapshotStatus = filterStatus;
		try {
			const resp = await api.search(query, buildFilters(results.length));
			// Discard if query or filters changed while loading
			if (query !== snapshotQuery || filterCollection !== snapshotCollection || filterStatus !== snapshotStatus) return;
			results = [...results, ...(resp.results ?? [])];
		} catch {
			// ignore
		} finally {
			loadingMore = false;
		}
	}

	function applyFilter(type: 'collection' | 'status', value: string) {
		if (type === 'collection') {
			filterCollection = filterCollection === value ? null : value;
		} else {
			filterStatus = filterStatus === value ? null : value;
		}
		doSearch();
	}

	function clearFilters() {
		filterCollection = null;
		filterStatus = null;
		doSearch();
	}

	function scrollSelectedIntoView() {
		requestAnimationFrame(() => {
			const el = document.querySelector('.result.selected');
			el?.scrollIntoView({ block: 'nearest' });
		});
	}

	function handleKeydown(e: KeyboardEvent) {
		if (e.key === 'Escape') {
			uiStore.closeSearch();
		} else if (e.key === 'ArrowDown') {
			e.preventDefault();
			selectedIdx = Math.min(selectedIdx + 1, flatResults.length - 1);
			scrollSelectedIntoView();
		} else if (e.key === 'ArrowUp') {
			e.preventDefault();
			selectedIdx = Math.max(selectedIdx - 1, 0);
			scrollSelectedIntoView();
		} else if (e.key === 'Enter' && flatResults.length > 0) {
			e.preventDefault();
			selectResult(flatResults[selectedIdx]);
		}
	}

	function selectResult(r: SearchResult) {
		saveRecentSearch(query.trim());
		const ws = workspaceStore.current?.slug;
		const wsUsername = workspaceStore.current?.owner_username;
		const collSlug = r.item.collection_slug;
		if (ws && wsUsername && collSlug) {
			goto(`/${wsUsername}/${ws}/${collSlug}/${itemUrlId(r.item)}`);
		}
		uiStore.closeSearch();
	}

	function useRecentSearch(q: string) {
		query = q;
		doSearch();
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

	function renderResultCard(r: SearchResult, i: number): { ref: string | null; status: string | undefined; priority: string | undefined } {
		return {
			ref: formatItemRef(r.item),
			status: getFieldValue(r.item, 'status'),
			priority: getFieldValue(r.item, 'priority')
		};
	}
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
					oninput={doSearch}
					placeholder="Search items, collections, docs..."
					class="search-input"
				/>
				{#if loading}
					<span class="search-spinner"></span>
				{/if}
				<kbd class="search-hint">esc</kbd>
			</div>

			<!-- Filter chips -->
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
</style>
