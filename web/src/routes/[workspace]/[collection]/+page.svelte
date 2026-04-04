<script lang="ts">
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { api } from '$lib/api/client';
	import type { Collection, Item, QuickAction, View, ViewConfig } from '$lib/types';
	import { parseSettings, parseFields, parseSchema, getStatusOptions, itemUrlId } from '$lib/types';
	import BoardView from '$lib/components/collections/BoardView.svelte';
	import ListView from '$lib/components/collections/ListView.svelte';
	import TableView from '$lib/components/collections/TableView.svelte';
	import FilterBar from '$lib/components/collections/FilterBar.svelte';
	import QuickActionsMenu from '$lib/components/common/QuickActionsMenu.svelte';
	import { onDestroy, onMount } from 'svelte';
	import { sseService } from '$lib/services/sse.svelte';
	import { visibility } from '$lib/services/visibility.svelte';
	import { toastStore } from '$lib/stores/toast.svelte';

	type ViewMode = 'list' | 'board' | 'table';

	let loading = $state(true);
	let collection = $state<Collection | null>(null);
	let items = $state<Item[]>([]);
	let viewMode = $state<ViewMode>('list');
	let activeFilters = $state<Record<string, string>>({});
	let searchQuery = $state('');
	let showArchived = $state(false);
	let itemProgress = $state<Record<string, { total: number; done: number }>>({});
	let progressLabel = $state('tasks');
	let relationLabels = $state<Record<string, string>>({});

	// Saved views state
	let savedViews = $state<View[]>([]);
	let activeViewId = $state<string | null>(null);
	let savingView = $state(false);
	let saveViewOpen = $state(false);
	let saveViewName = $state('');
	let saveViewInput = $state<HTMLInputElement>();

	let wsSlug = $derived(page.params.workspace ?? '');
	let collSlug = $derived(page.params.collection ?? '');

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
		if (wsSlug && collSlug) loadCollection(wsSlug, collSlug, showArchived);
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

	// Silently refresh items when the tab regains focus (SSE events may have been lost)
	let unsubscribeVisibility: (() => void) | null = null;

	onMount(() => {
		unsubscribeVisibility = visibility.onTabResume(async () => {
			if (!wsSlug || !collSlug) return;
			try {
				const listParams = showArchived ? { include_archived: true } : undefined;
				const freshItems = await api.items.listByCollection(wsSlug, collSlug, listParams);
				items = freshItems;

				// Update progress data without resetting view state
				if (collSlug === 'phases') {
					const progress = await api.items.phasesProgress(wsSlug).catch(() => []);
					const map: Record<string, { total: number; done: number }> = {};
					for (const p of progress) {
						map[p.phase_id] = { total: p.total, done: p.done };
					}
					itemProgress = map;
				} else {
					const map: Record<string, { total: number; done: number }> = {};
					for (const it of freshItems) {
						if (!it.content) continue;
						const total = (it.content.match(/- \[[ x]\]/g) ?? []).length;
						if (total === 0) continue;
						const done = (it.content.match(/- \[x\]/g) ?? []).length;
						map[it.id] = { total, done };
					}
					itemProgress = map;
				}
			} catch {
				// Ignore — will catch up on next SSE event
			}
		});
	});

	onDestroy(() => {
		unsubscribeSSE?.();
		unsubscribeVisibility?.();
	});

	async function loadCollection(ws: string, coll: string, includeArchived = false) {
		loading = true;
		try {
			const listParams = includeArchived ? { include_archived: true } : undefined;
			const [collData, itemsData, viewsData] = await Promise.all([
				api.collections.get(ws, coll),
				api.items.listByCollection(ws, coll, listParams),
				api.views.list(ws, coll).catch(() => [] as View[])
			]);
			collection = collData;
			items = itemsData;
			savedViews = viewsData;
			activeViewId = null;

			// Fetch phase progress if viewing phases collection
			if (coll === 'phases') {
				try {
					const progress = await api.items.phasesProgress(ws);
					const map: Record<string, { total: number; done: number }> = {};
					for (const p of progress) {
						map[p.phase_id] = { total: p.total, done: p.done };
					}
					itemProgress = map;
					progressLabel = 'tasks';
				} catch {
					itemProgress = {};
				}
			} else {
				// Compute checklist progress from item content (markdown checkboxes)
				const map: Record<string, { total: number; done: number }> = {};
				for (const it of itemsData) {
					if (!it.content) continue;
					const total = (it.content.match(/- \[[ x]\]/g) ?? []).length;
					if (total === 0) continue;
					const done = (it.content.match(/- \[x\]/g) ?? []).length;
					map[it.id] = { total, done };
				}
				itemProgress = map;
				progressLabel = 'done';
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
			const defaultMode = (['board', 'list', 'table'].includes(settings.default_view))
				? settings.default_view as ViewMode : 'list';
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
				// Phase filter uses the phase link, not fields JSON
				if (key === 'phase') {
					return item.phase_id === value;
				}
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

	const emptyHintMap: Record<string, string> = {
		tasks: '/pad break down my current work into tasks',
		ideas: "/pad I have an idea for...",
		phases: '/pad create a phase for what I\'m working on',
		docs: '/pad document the architecture of this project',
		conventions: '/pad what conventions should this project follow?',
		playbooks: '/pad set up playbooks for our workflow',
		bugs: '/pad triage open issues in this project',
	};

	let emptyHint = $derived(emptyHintMap[collSlug] ?? null);

	let filtersOpen = $state(false);
	let hasActiveFilters = $derived(searchQuery.trim() !== '' || Object.keys(activeFilters).length > 0);

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
			const updated = await api.items.update(wsSlug, item.id, {
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
			const item = items.find((i) => i.slug === slug || i.id === slug);
			if (item) {
				item.sort_order = sort_order;
			}
		}
		// Persist to API sequentially (SQLite can't handle concurrent writes)
		try {
			for (const { slug, sort_order } of updates) {
				const item = items.find((i) => i.slug === slug || i.id === slug);
				await api.items.update(wsSlug, item?.id ?? slug, { sort_order });
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
			goto(`/${wsSlug}/${collSlug}/${itemUrlId(item)}?new=1`);
		} catch {
			toastStore.show('Failed to create item', 'error');
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
			items = [...items, item];
			quickCreateTitle = '';
			toastStore.show(`Created "${title}"`, 'success');
		} catch {
			toastStore.show('Failed to create item', 'error');
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
					goto(`/${wsSlug}/${collSlug}/${itemUrlId(item)}`);
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
			items = items.filter((i) => !itemsToArchive.some((a) => a.id === i.id));
			toastStore.show(`Archived ${count} item${count !== 1 ? 's' : ''}`, 'success');
		} catch {
			toastStore.show('Failed to archive some items', 'error');
		}
	}

	async function handleRestore(item: Item) {
		if (!wsSlug) return;
		try {
			const restored = await api.items.restore(wsSlug, item.id);
			const idx = items.findIndex((i) => i.id === item.id);
			if (idx !== -1) {
				items[idx] = restored;
			}
			toastStore.show(`Restored "${item.title}"`, 'success');
		} catch {
			toastStore.show('Failed to restore item', 'error');
		}
	}

	// --- Saved views ---

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

					{#if quickActions.length > 0 && collection}
						<QuickActionsMenu actions={quickActions} {collection} scope="collection" />
					{/if}

					<button class="new-btn" onclick={handleNewButtonClick} disabled={creatingNew}>
						+ <span class="new-btn-label">New {singularName()}</span>
					</button>
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

			{#if quickCreateOpen}
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
		{#if items.length === 0}
			<div class="empty-state-box">
				<div class="empty-icon">{collection.icon || '📦'}</div>
				<h2>No {collection.name.toLowerCase()} yet</h2>
				<p>Create your first {singularName().toLowerCase()} to get started.</p>
				<a href="/{wsSlug}/new?collection={collSlug}" class="empty-cta">+ Create {singularName()}</a>
				{#if emptyHint}
					<p class="empty-hint">Or try: <code>{emptyHint}</code></p>
				{/if}
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
				{wsSlug}
				{groupField}
				{focusedItemId}
				onStatusChange={handleStatusChange}
				onReorder={handleReorder}
				onArchiveColumn={handleBulkArchive}
				onGroupReorder={handleGroupReorder}
				oncreate={openQuickCreate}
				{itemProgress}
				{progressLabel}
				{relationLabels}
			/>
		{:else if viewMode === 'table'}
			<TableView
				items={filteredItems}
				{collection}
				{wsSlug}
				onStatusChange={handleStatusChange}
				oncreate={openQuickCreate}
				{itemProgress}
				{progressLabel}
				{relationLabels}
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
				oncreate={openQuickCreate}
				{itemProgress}
				{progressLabel}
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
	}
</style>
