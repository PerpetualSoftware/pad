<script lang="ts">
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { api } from '$lib/api/client';
	import type { Collection, Item } from '$lib/types';
	import { parseSettings, parseFields, parseSchema, getStatusOptions } from '$lib/types';
	import BoardView from '$lib/components/collections/BoardView.svelte';
	import ListView from '$lib/components/collections/ListView.svelte';
	import FilterBar from '$lib/components/collections/FilterBar.svelte';
	import { onDestroy } from 'svelte';
	import { sseService } from '$lib/services/sse.svelte';
	import { toastStore } from '$lib/stores/toast.svelte';

	type ViewMode = 'list' | 'board';

	let loading = $state(true);
	let collection = $state<Collection | null>(null);
	let items = $state<Item[]>([]);
	let viewMode = $state<ViewMode>('list');
	let activeFilters = $state<Record<string, string>>({});
	let searchQuery = $state('');
	let itemProgress = $state<Record<string, { total: number; done: number }>>({});
	let relationLabels = $state<Record<string, string>>({});

	let wsSlug = $derived(page.params.workspace);
	let collSlug = $derived(page.params.collection);

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
			if (saved === 'list' || saved === 'board') return saved;
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
		const newUrl = `/${wsSlug}/${collSlug}${qs ? '?' + qs : ''}`;
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
		if (wsSlug && collSlug) loadCollection(wsSlug, collSlug);
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
			// Only react to events for this collection
			if (event.collection !== coll) return;

			switch (event.type) {
				case 'item_created':
				case 'item_archived':
				case 'item_restored': {
					try {
						items = await api.items.listByCollection(ws, coll);
					} catch {
						// Ignore fetch errors — will retry on next event
					}
					break;
				}
				case 'item_updated': {
					try {
						items = await api.items.listByCollection(ws, coll);
					} catch {
						// Ignore fetch errors
					}
					break;
				}
			}
		});
	});

	onDestroy(() => {
		unsubscribeSSE?.();
	});

	async function loadCollection(ws: string, coll: string) {
		loading = true;
		try {
			const [collData, itemsData] = await Promise.all([
				api.collections.get(ws, coll),
				api.items.listByCollection(ws, coll)
			]);
			collection = collData;
			items = itemsData;

			// Fetch phase progress if viewing phases collection
			if (coll === 'phases') {
				try {
					const progress = await api.items.phasesProgress(ws);
					const map: Record<string, { total: number; done: number }> = {};
					for (const p of progress) {
						map[p.phase_id] = { total: p.total, done: p.done };
					}
					itemProgress = map;
				} catch {
					itemProgress = {};
				}
			} else {
				itemProgress = {};
			}

			// Fetch phase names for relation display on task cards
			if (coll === 'tasks') {
				try {
					const phases = await api.items.listByCollection(ws, 'phases');
					const labels: Record<string, string> = {};
					for (const p of phases) {
						labels[p.id] = p.title;
					}
					relationLabels = labels;
				} catch {
					relationLabels = {};
				}
			} else {
				relationLabels = {};
			}

			// Set view mode: URL param > localStorage > collection default
			const settings = parseSettings(collData);
			const defaultMode = (settings.default_view === 'board' || settings.default_view === 'list')
				? settings.default_view : 'list';
			viewMode = loadSavedViewMode(coll, defaultMode);

			// Override with URL params if present
			loadUrlFilters();
		} catch {
			collection = null;
			items = [];
		} finally {
			loading = false;
		}
	}

	let settings = $derived(collection ? parseSettings(collection) : null);
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
				const fields = parseFields(item);
				return fields[key] === value;
			});
		}

		// Apply search query
		if (searchQuery.trim()) {
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
		searchQuery = query;
		updateUrlFilters();
	}

	async function handleStatusChange(item: Item, newValue: string) {
		if (!wsSlug) return;
		const fields = parseFields(item);
		fields[groupField] = newValue;
		try {
			const updated = await api.items.update(wsSlug, item.slug, {
				fields: JSON.stringify(fields)
			});
			// Replace item in-place
			const idx = items.findIndex((i) => i.id === item.id);
			if (idx !== -1) {
				items[idx] = updated;
			}
			toastStore.show(`Moved to ${formatLabel(newValue)}`, 'success');
		} catch (e) {
			console.error('Failed to update item:', e);
			toastStore.show('Failed to update status', 'error');
		}
	}

	async function handleReorder(updates: { slug: string; sort_order: number }[]) {
		if (!wsSlug) return;
		// Optimistically update local sort_order values
		for (const { slug, sort_order } of updates) {
			const idx = items.findIndex((i) => i.slug === slug);
			if (idx !== -1) {
				items[idx] = { ...items[idx], sort_order };
			}
		}
		// Persist to API sequentially (SQLite can't handle concurrent writes)
		try {
			for (const { slug, sort_order } of updates) {
				await api.items.update(wsSlug, slug, { sort_order });
			}
		} catch (e) {
			console.error('Failed to persist sort order:', e);
		}
	}

	function formatLabel(value: string): string {
		return value.replace(/_/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase());
	}
</script>

<div class="collection-page">
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
				</h1>
				{#if itemCounts}
					<span class="summary-stats">
						{#each Object.entries(itemCounts) as [status, count] (status)}
							<span class="stat">{count} {formatLabel(status).toLowerCase()}</span>
							{#if status !== Object.keys(itemCounts).at(-1)}
								<span class="stat-sep">&middot;</span>
							{/if}
						{/each}
					</span>
				{/if}
			</div>

			<div class="controls-row">
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
				</div>

				<FilterBar
					{collection}
					{activeFilters}
					{searchQuery}
					onFilterChange={handleFilterChange}
					onSearchChange={handleSearchChange}
					{relationLabels}
				/>

				<a href="/{wsSlug}/new?collection={collSlug}" class="new-btn">
					+ New {singularName()}
				</a>
			</div>
		</div>

		<!-- Content -->
		{#if items.length === 0}
			<div class="empty-state-box">
				<div class="empty-icon">{collection.icon || '📦'}</div>
				<h2>No {collection.name.toLowerCase()} yet</h2>
				<p>Create your first {singularName().toLowerCase()} to get started.</p>
				<a href="/{wsSlug}/{collSlug}/new" class="empty-cta">+ Create {singularName()}</a>
			</div>
		{:else if filteredItems.length === 0 && (searchQuery || Object.keys(activeFilters).length > 0)}
			<div class="empty-state-box">
				<div class="empty-icon">🔍</div>
				<h2>No matches</h2>
				<p>No items match your current filters.
					<button class="clear-link" onclick={() => { activeFilters = {}; searchQuery = ''; }}>Clear filters</button>
				</p>
			</div>
		{:else if viewMode === 'board'}
			<BoardView
				items={filteredItems}
				{collection}
				{groupField}
				onStatusChange={handleStatusChange}
				onReorder={handleReorder}
				{itemProgress}
				{relationLabels}
			/>
		{:else}
			<ListView
				items={filteredItems}
				{collection}
				{groupField}
				{statusOptions}
				onStatusChange={handleStatusChange}
				onReorder={handleReorder}
				{itemProgress}
				{relationLabels}
			/>
		{/if}
	{/if}
</div>

<style>
	.collection-page {
		max-width: var(--content-max-width);
		margin: 0 auto;
		padding: var(--space-8) var(--space-6);
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
		margin-bottom: var(--space-6);
	}

	.title-row {
		display: flex;
		align-items: baseline;
		gap: var(--space-4);
		margin-bottom: var(--space-4);
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

	.summary-stats {
		font-size: 0.85em;
		color: var(--text-secondary);
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}

	.stat {
		color: var(--text-secondary);
	}

	.stat-sep {
		color: var(--text-muted);
	}

	/* Controls */
	.controls-row {
		display: flex;
		align-items: flex-start;
		gap: var(--space-4);
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
		padding: var(--space-2) var(--space-3);
		cursor: pointer;
		font-size: 1em;
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

	@media (max-width: 768px) {
		.controls-row {
			flex-direction: column;
			align-items: flex-start;
		}
	}
</style>
