<script lang="ts">
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { api } from '$lib/api/client';
	import type { Collection, Item, QuickAction } from '$lib/types';
	import { parseSettings, parseFields, parseSchema, getStatusOptions, itemUrlId } from '$lib/types';
	import BoardView from '$lib/components/collections/BoardView.svelte';
	import ListView from '$lib/components/collections/ListView.svelte';
	import TableView from '$lib/components/collections/TableView.svelte';
	import FilterBar from '$lib/components/collections/FilterBar.svelte';
	import QuickActionsMenu from '$lib/components/common/QuickActionsMenu.svelte';
	import { onDestroy } from 'svelte';
	import { sseService } from '$lib/services/sse.svelte';
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

	onDestroy(() => {
		unsubscribeSSE?.();
	});

	async function loadCollection(ws: string, coll: string, includeArchived = false) {
		loading = true;
		try {
			const listParams = includeArchived ? { include_archived: true } : undefined;
			const [collData, itemsData] = await Promise.all([
				api.collections.get(ws, coll),
				api.items.listByCollection(ws, coll, listParams)
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
		if (quickCreateOpen) return;

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
					<button
						class="toggle-btn"
						class:active={viewMode === 'table'}
						onclick={() => { saveViewMode('table'); updateUrlFilters(); }}
						aria-label="Table view"
						title="Table view"
					>&#9783;</button>
				</div>

				<FilterBar
					{collection}
					{activeFilters}
					{searchQuery}
					onFilterChange={handleFilterChange}
					onSearchChange={handleSearchChange}
					{relationLabels}
				/>

				<label class="archive-toggle">
					<input type="checkbox" bind:checked={showArchived} />
					<span>Show archived</span>
				</label>

				{#if quickActions.length > 0 && collection}
					<QuickActionsMenu actions={quickActions} {collection} scope="collection" />
				{/if}

				<button class="new-btn" onclick={openQuickCreate} disabled={creatingNew}>
					+ New {singularName()}
				</button>
			</div>

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
		</div>

		<!-- Content -->
		{#if items.length === 0}
			<div class="empty-state-box">
				<div class="empty-icon">{collection.icon || '📦'}</div>
				<h2>No {collection.name.toLowerCase()} yet</h2>
				<p>Create your first {singularName().toLowerCase()} to get started.</p>
				<a href="/{wsSlug}/{collSlug}/new" class="empty-cta">+ Create {singularName()}</a>
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
				{groupField}
				{focusedItemId}
				onStatusChange={handleStatusChange}
				onReorder={handleReorder}
				onArchiveColumn={handleBulkArchive}
				onGroupReorder={handleGroupReorder}
				{itemProgress}
				{progressLabel}
				{relationLabels}
			/>
		{:else if viewMode === 'table'}
			<TableView
				items={filteredItems}
				{collection}
				onStatusChange={handleStatusChange}
				{itemProgress}
				{progressLabel}
				{relationLabels}
			/>
		{:else}
			<ListView
				items={filteredItems}
				{collection}
				{groupField}
				{focusedItemId}
				{statusOptions}
				onStatusChange={handleStatusChange}
				onReorder={handleReorder}
				onArchiveGroup={handleBulkArchive}
				onGroupReorder={handleGroupReorder}
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

	@media (max-width: 768px) {
		.controls-row {
			flex-direction: column;
			align-items: flex-start;
		}
	}
</style>
