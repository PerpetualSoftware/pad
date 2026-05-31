<script lang="ts">
	import { page } from '$app/state';
	import { onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import { marked } from 'marked';
	import DOMPurify from 'dompurify';
	import PublicCollectionView from '$lib/components/share/PublicCollectionView.svelte';
	import {
		parsePublicCollection,
		parsePublicItems,
		type PublicCollection,
		type PublicItem
	} from '$lib/components/share/shareView';
	import type { PublicShareCollection, PublicShareItem, PublicShareView } from '$lib/types';

	let token = $derived(page.params.token ?? '');

	// Read-only view switcher (TASK-1682 / PLAN-1677 Phase 2). The anonymous
	// viewer toggles between the three base view types (and any of the
	// collection's saved views). The choice is purely presentational — it
	// drives PublicCollectionView's `view` prop, which overrides the owner's
	// `settings.default_view`. No server save (read-only context); we mirror
	// the logged-in collection page's URL-param + localStorage persistence.
	type BaseView = 'list' | 'board' | 'table';
	const BASE_VIEWS: { value: BaseView; label: string; icon: string }[] = [
		{ value: 'list', label: 'List', icon: '☰' },
		{ value: 'board', label: 'Board', icon: '▦' },
		{ value: 'table', label: 'Table', icon: '⚏' }
	];

	// Selection is a discriminated handle: either a base view type, or a saved
	// view addressed by slug (which resolves to a base view_type for rendering).
	let selectedKind = $state<'base' | 'saved'>('base');
	let selectedBase = $state<BaseView>('list');
	let selectedSavedSlug = $state('');

	let loading = $state(true);
	let error = $state('');
	let requireAuth = $state(false);
	let requirePassword = $state(false);
	let passwordInput = $state('');
	let passwordError = $state('');
	let passwordLoading = $state(false);

	let shareType = $state<'item' | 'collection' | ''>('');
	let itemData = $state<{
		title: string;
		fields: Record<string, any>;
		content: string;
		collection_name?: string;
		collection_icon?: string;
		item_ref?: string;
	} | null>(null);
	// Raw `collection` + `items` branches of the share payload, fed straight to
	// PublicCollectionView (which parses settings/schema/fields defensively and
	// renders the owner's chosen view type). `name` is also used for the page
	// title. Null until a collection payload has loaded.
	let collectionData = $state<{
		collection: PublicShareCollection;
		items: PublicShareItem[];
	} | null>(null);

	// Saved views from the share payload (empty array when the collection has
	// none). Sorted by the owner's `sort_order` for stable presentation.
	let savedViews = $derived.by(() => {
		const views = collectionData?.collection.views ?? [];
		return [...views].sort((a, b) => a.sort_order - b.sort_order);
	});

	// Owner's default base view, the fallback when nothing is selected.
	let defaultView = $derived<BaseView>(
		coerceBaseView(collectionData?.collection.settings?.default_view) ?? 'list'
	);

	// The effective base view fed to PublicCollectionView. A saved-view
	// selection resolves through its `view_type`; an unknown/stale saved slug
	// falls back to the owner default.
	// The saved view object backing the current selection (null in base mode or
	// when the slug is stale).
	let activeSavedView = $derived.by<PublicShareView | null>(() => {
		if (selectedKind !== 'saved') return null;
		return savedViews.find((sv) => sv.slug === selectedSavedSlug) ?? null;
	});

	let activeBaseView = $derived.by<BaseView>(() => {
		if (selectedKind === 'saved') {
			if (activeSavedView) return coerceBaseView(activeSavedView.view_type) ?? defaultView;
			return defaultView;
		}
		return selectedBase;
	});

	// Parse the raw payload once into the renderer's PublicCollection /
	// PublicItem shapes (string-or-object tolerant). When a saved view is
	// active, overlay its config: grouping is mapped onto the matching
	// settings knob (board → board_group_by, list → list_group_by) and the
	// config's filters narrow the item set. Read-only — purely presentational,
	// no server round-trip. Base-view selections render the unfiltered
	// collection exactly as before.
	let baseParsedCollection = $derived.by<PublicCollection>(() =>
		parsePublicCollection(collectionData?.collection)
	);
	let baseParsedItems = $derived.by<PublicItem[]>(() =>
		parsePublicItems(collectionData?.items)
	);

	let effectiveCollection = $derived.by<PublicCollection>(() => {
		const coll = baseParsedCollection;
		const view = activeSavedView;
		if (!view) return coll;
		const groupBy = typeof view.config?.group_by === 'string' ? view.config.group_by : '';
		if (!groupBy) return coll;
		// Clone settings so we don't mutate the shared parsed object; route the
		// group key to the knob the active base renderer actually reads.
		const settings = { ...coll.settings };
		if (activeBaseView === 'board') settings.board_group_by = groupBy;
		else if (activeBaseView === 'list') settings.list_group_by = groupBy;
		return { ...coll, settings };
	});

	let effectiveItems = $derived.by<PublicItem[]>(() => {
		const items = baseParsedItems;
		const view = activeSavedView;
		if (!view) return items;
		const rawFilters = Array.isArray(view.config?.filters) ? view.config.filters : [];
		// Only apply filters we can actually evaluate against the public
		// payload. Share items carry schema `fields` only — NOT the logged-in
		// page's top-level `tags` / `parent_link_id` / `phase`. A filter on one
		// of those would otherwise hide the entire collection (Codex round 2).
		// Evaluability is decided against the COLLECTION SCHEMA (not item
		// values): a `priority eq high` filter on a schema that HAS `priority`
		// stays applied even when no shared item happens to set it (so the
		// public view returns none, matching the owner) — Codex round 3. Only
		// fields genuinely absent from the public schema are skipped.
		const schemaKeys = new Set(baseParsedCollection.fields.map((f) => f.key));
		const applicable = rawFilters.filter((f) => filterEvaluable(schemaKeys, f));
		if (applicable.length === 0) return items;
		return items.filter((item) => applicable.every((f) => matchesFilter(item, f)));
	});

	// True when the filter targets a field present in the public collection
	// schema, so it's meaningful against the shared payload. Malformed/fieldless
	// filters are treated as evaluable so matchesFilter can no-op them.
	function filterEvaluable(schemaKeys: Set<string>, filter: unknown): boolean {
		if (!filter || typeof filter !== 'object') return true;
		const field = (filter as { field?: unknown }).field;
		if (typeof field !== 'string') return true;
		return schemaKeys.has(field);
	}

	// Read-only filter evaluation mirroring the logged-in collection page's
	// `eq` / `in` handling (the only ops saved-view configs emit). Unknown ops
	// pass through (don't hide items we can't reason about).
	function matchesFilter(item: PublicItem, filter: unknown): boolean {
		if (!filter || typeof filter !== 'object') return true;
		const f = filter as { field?: unknown; op?: unknown; value?: unknown };
		if (typeof f.field !== 'string') return true;
		const fieldVal = item.fields[f.field];
		if (f.op === 'eq') {
			return String(fieldVal ?? '') === String(f.value ?? '');
		}
		if (f.op === 'in') {
			const wanted = Array.isArray(f.value) ? f.value.map((v) => String(v)) : [String(f.value)];
			// Tags-style array fields: match if any item value is wanted.
			if (Array.isArray(fieldVal)) {
				return fieldVal.some((v) => wanted.includes(String(v)));
			}
			return wanted.includes(String(fieldVal ?? ''));
		}
		return true;
	}

	function coerceBaseView(value: unknown): BaseView | null {
		if (value === 'list' || value === 'board' || value === 'table') return value;
		// Saved views may use 'kanban' as a synonym for the board renderer.
		if (value === 'kanban') return 'board';
		return null;
	}

	// localStorage persistence keyed by share token (mirrors the logged-in
	// collection page's `pad-view-<coll>` pattern). Stores either a base view
	// type or `saved:<slug>`. Wrapped in try/catch — share pages may run in
	// privacy contexts where localStorage throws.
	function viewStorageKey() {
		return token ? `pad-share-view-${token}` : '';
	}

	function saveSelection() {
		const key = viewStorageKey();
		if (!key) return;
		const handle = selectedKind === 'saved' ? `saved:${selectedSavedSlug}` : selectedBase;
		try {
			localStorage.setItem(key, handle);
		} catch {}
	}

	function loadSavedSelection(): string | null {
		const key = viewStorageKey();
		if (!key) return null;
		try {
			return localStorage.getItem(key);
		} catch {
			return null;
		}
	}

	// Apply a stored/URL handle to the selection state. Returns true if it
	// resolved to a known view; false (caller falls back to the owner default)
	// for an empty, malformed, or stale-saved handle.
	function applyViewHandle(handle: string | null | undefined): boolean {
		if (!handle) return false;
		if (handle.startsWith('saved:')) {
			const slug = handle.slice('saved:'.length);
			if (savedViews.some((v) => v.slug === slug)) {
				selectedKind = 'saved';
				selectedSavedSlug = slug;
				selectedBase = coerceBaseView(savedViews.find((v) => v.slug === slug)?.view_type) ?? defaultView;
				return true;
			}
			return false;
		}
		const base = coerceBaseView(handle);
		if (base) {
			selectedKind = 'base';
			selectedBase = base;
			selectedSavedSlug = '';
			return true;
		}
		return false;
	}

	// Resolve the initial selection: URL `?view=` param > localStorage >
	// owner default. Called once after a collection payload loads.
	function initSelection() {
		selectedBase = defaultView;
		selectedKind = 'base';
		selectedSavedSlug = '';

		const urlHandle = page.url.searchParams.get('view');
		const fromUrl = applyViewHandle(urlHandle);
		if (!fromUrl) {
			applyViewHandle(loadSavedSelection());
			// Nothing valid → owner default already set above.
		}
		// Normalize the address bar to the resolved selection. A stale/invalid
		// `?view=` (e.g. a deleted saved view) otherwise leaves the URL
		// describing a view the page isn't rendering, so a copied link
		// wouldn't reproduce what the viewer sees (Codex round 3). When the URL
		// already matched, this is a harmless no-op replaceState.
		syncUrl();
	}

	// Mirror the active selection into the URL (`?view=`), replaceState so the
	// switcher never pollutes history. Drop the param only when the base
	// selection equals the owner default — that's the no-op case, and the
	// reload-without-localStorage path resolves back to the same view. A
	// non-default base (e.g. List on a board-default share) MUST keep the
	// param so the link reproduces the viewer's choice (Codex round 2).
	function syncUrl() {
		const url = new URL(page.url);
		const handle = selectedKind === 'saved' ? `saved:${selectedSavedSlug}` : selectedBase;
		if (selectedKind === 'base' && selectedBase === defaultView) {
			url.searchParams.delete('view');
		} else {
			url.searchParams.set('view', handle);
		}
		history.replaceState(history.state, '', url);
	}

	function selectBaseView(base: BaseView) {
		selectedKind = 'base';
		selectedBase = base;
		selectedSavedSlug = '';
		saveSelection();
		syncUrl();
	}

	// Human label for a saved view's underlying base view type (used in the
	// chip tooltip so viewers know what a saved view renders as).
	function activeViewTypeLabel(viewType: string): string {
		const base = coerceBaseView(viewType) ?? defaultView;
		return BASE_VIEWS.find((b) => b.value === base)?.label ?? 'List';
	}

	function selectSavedView(slug: string) {
		selectedKind = 'saved';
		selectedSavedSlug = slug;
		selectedBase = coerceBaseView(savedViews.find((v) => v.slug === slug)?.view_type) ?? defaultView;
		saveSelection();
		syncUrl();
	}

	let renderedContent = $derived.by(() => {
		if (!itemData?.content) return '';
		try {
			const raw = marked(itemData.content) as string;
			return typeof window !== 'undefined' ? DOMPurify.sanitize(raw) : raw;
		} catch {
			// Sanitize the fallback too — never pass user content to {@html} raw
			const fallback = itemData.content;
			return typeof window !== 'undefined' ? DOMPurify.sanitize(fallback) : fallback;
		}
	});

	onMount(async () => {
		if (!token) {
			error = 'Invalid share link.';
			loading = false;
			return;
		}

		try {
			const data = await api.share.get(token);

			if (data.require_auth) {
				requireAuth = true;
				loading = false;
				return;
			}

			if (data.require_password) {
				requirePassword = true;
				loading = false;
				return;
			}

			if (data.type === 'item') {
				shareType = 'item';
				let fields: Record<string, any> = {};
				if (data.item?.fields) {
					try {
						fields = typeof data.item.fields === 'string' ? JSON.parse(data.item.fields) : data.item.fields;
					} catch {
						fields = {};
					}
				}
				itemData = {
					title: data.item?.title ?? 'Untitled',
					fields,
					content: data.item?.content ?? '',
					collection_name: data.item?.collection_name,
					collection_icon: data.item?.collection_icon,
					item_ref: data.item?.ref ?? data.item?.item_ref
				};
			} else if (data.type === 'collection') {
				shareType = 'collection';
				collectionData = {
					collection: data.collection ?? { name: 'Collection' },
					items: data.items ?? []
				};
				initSelection();
			} else {
				error = 'Unknown share type.';
			}
		} catch (e: any) {
			if (e.code === 'unauthorized' || e.code === 'auth_required') {
				requireAuth = true;
			} else {
				error = e.message ?? 'Failed to load shared content.';
			}
		} finally {
			loading = false;
		}
	});

	function formatFieldValue(value: unknown): string {
		if (value === null || value === undefined) return '';
		if (Array.isArray(value)) return value.join(', ');
		return String(value);
	}

	function formatFieldLabel(key: string): string {
		return key.replace(/_/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase());
	}

	async function submitPassword() {
		passwordError = '';
		passwordLoading = true;
		try {
			const resp = await fetch(`/api/v1/s/${token}`, {
				credentials: 'same-origin',
				headers: { 'X-Share-Password': passwordInput }
			});
			const data = await resp.json();
			if (!resp.ok) {
				passwordError = data?.error?.message ?? 'Incorrect password';
				return;
			}
			if (data.require_password) {
				passwordError = 'Incorrect password';
				return;
			}
			// Re-process the data exactly like onMount does
			requirePassword = false;
			if (data.type === 'item') {
				shareType = 'item';
				let fields: Record<string, any> = {};
				if (data.item?.fields) {
					try {
						fields = typeof data.item.fields === 'string' ? JSON.parse(data.item.fields) : data.item.fields;
					} catch { fields = {}; }
				}
				itemData = {
					title: data.item?.title ?? 'Untitled',
					fields,
					content: data.item?.content ?? '',
					collection_name: data.item?.collection_name,
					collection_icon: data.item?.collection_icon,
					item_ref: data.item?.ref ?? data.item?.item_ref
				};
			} else if (data.type === 'collection') {
				shareType = 'collection';
				collectionData = {
					collection: data.collection ?? { name: 'Collection' },
					items: data.items ?? []
				};
				initSelection();
			}
		} catch (e: any) {
			passwordError = e.message ?? 'Failed to verify password';
		} finally {
			passwordLoading = false;
		}
	}
</script>

<svelte:head>
	{#if itemData}
		<title>{itemData.title} - Shared via Pad</title>
	{:else if collectionData}
		<title>{collectionData.collection.name ?? 'Collection'} - Shared via Pad</title>
	{:else}
		<title>Shared - Pad</title>
	{/if}
</svelte:head>

<div class="share-page">
	<div class="share-container">
		{#if loading}
			<div class="share-loading">
				<div class="loading-spinner"></div>
				<p>Loading shared content...</p>
			</div>
		{:else if requireAuth}
			<div class="share-auth">
				<div class="auth-icon">
					<svg width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round">
						<rect x="3" y="11" width="18" height="11" rx="2" ry="2"/>
						<path d="M7 11V7a5 5 0 0 1 10 0v4"/>
					</svg>
				</div>
				<h1>Sign in to view</h1>
				<p>This shared content requires authentication.</p>
				<a href="/login" class="auth-link">Sign in</a>
			</div>
		{:else if requirePassword}
			<div class="share-auth">
				<div class="auth-icon">
					<svg width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round">
						<rect x="3" y="11" width="18" height="11" rx="2" ry="2"/>
						<path d="M7 11V7a5 5 0 0 1 10 0v4"/>
					</svg>
				</div>
				<h1>Password required</h1>
				<p>Enter the password to view this shared content.</p>
				<form class="password-form" onsubmit={(e) => { e.preventDefault(); submitPassword(); }}>
					<input
						type="password"
						bind:value={passwordInput}
						placeholder="Enter password"
						class="password-input"
						disabled={passwordLoading}
					/>
					{#if passwordError}
						<p class="password-error">{passwordError}</p>
					{/if}
					<button type="submit" class="auth-link" disabled={passwordLoading || !passwordInput}>
						{passwordLoading ? 'Verifying...' : 'View content'}
					</button>
				</form>
			</div>
		{:else if error}
			<div class="share-error">
				<h1>Unable to load</h1>
				<p>{error}</p>
			</div>
		{:else if shareType === 'item' && itemData}
			<article class="share-item">
				{#if itemData.collection_name}
					<div class="item-collection-badge">
						{#if itemData.collection_icon}
							<span class="collection-icon">{itemData.collection_icon}</span>
						{/if}
						<span>{itemData.collection_name}</span>
						{#if itemData.item_ref}
							<span class="item-ref">{itemData.item_ref}</span>
						{/if}
					</div>
				{/if}

				<h1 class="item-title">{itemData.title}</h1>

				{#if Object.keys(itemData.fields).length > 0}
					<div class="item-fields">
						{#each Object.entries(itemData.fields) as [key, value] (key)}
							{#if value !== null && value !== undefined && value !== ''}
								<div class="field-chip">
									<span class="field-chip-label">{formatFieldLabel(key)}</span>
									<span class="field-chip-value">{formatFieldValue(value)}</span>
								</div>
							{/if}
						{/each}
					</div>
				{/if}

				{#if itemData.content}
					<div class="item-content">
						{@html renderedContent}
					</div>
				{/if}
			</article>
		{:else if shareType === 'collection' && collectionData}
			<div class="collection-toolbar">
				<div class="view-toggle" role="group" aria-label="Select view">
					{#each BASE_VIEWS as bv (bv.value)}
						<button
							type="button"
							class="toggle-btn"
							class:active={selectedKind === 'base' && selectedBase === bv.value}
							aria-pressed={selectedKind === 'base' && selectedBase === bv.value}
							aria-label={`${bv.label} view`}
							title={`${bv.label} view`}
							onclick={() => selectBaseView(bv.value)}
						>
							<span class="toggle-icon" aria-hidden="true">{bv.icon}</span>
							<span class="toggle-label">{bv.label}</span>
						</button>
					{/each}
				</div>

				{#if savedViews.length > 0}
					<div class="saved-views" role="group" aria-label="Saved views">
						<span class="saved-views-label">Views</span>
						{#each savedViews as sv (sv.slug)}
							<button
								type="button"
								class="saved-view-chip"
								class:active={selectedKind === 'saved' && selectedSavedSlug === sv.slug}
								aria-pressed={selectedKind === 'saved' && selectedSavedSlug === sv.slug}
								title={`${sv.name} (${activeViewTypeLabel(sv.view_type)})`}
								onclick={() => selectSavedView(sv.slug)}
							>
								{sv.name}
							</button>
						{/each}
					</div>
				{/if}
			</div>

			<PublicCollectionView
				parsedCollection={effectiveCollection}
				parsedItems={effectiveItems}
				view={activeBaseView}
				expandable={false}
			/>
		{/if}
	</div>

	<footer class="share-footer">
		<span>Powered by <a href="https://getpad.dev" target="_blank" rel="noopener noreferrer">Pad</a></span>
	</footer>
</div>

<style>
	.share-page {
		min-height: 100vh;
		display: flex;
		flex-direction: column;
		background: var(--bg-primary);
		color: var(--text-primary);
	}

	.share-container {
		flex: 1;
		width: 100%;
		max-width: 780px;
		margin: 0 auto;
		padding: var(--space-8) var(--space-5);
	}

	/* Loading */
	.share-loading {
		display: flex;
		flex-direction: column;
		align-items: center;
		gap: var(--space-4);
		padding: var(--space-10) 0;
		color: var(--text-muted);
	}

	.loading-spinner {
		width: 32px;
		height: 32px;
		border: 2px solid var(--border);
		border-top-color: var(--accent-blue);
		border-radius: 50%;
		animation: spin 0.8s linear infinite;
	}

	@keyframes spin {
		to { transform: rotate(360deg); }
	}

	/* Auth required */
	.share-auth {
		display: flex;
		flex-direction: column;
		align-items: center;
		gap: var(--space-4);
		padding: var(--space-10) 0;
		text-align: center;
	}

	.auth-icon {
		color: var(--text-muted);
	}

	.share-auth h1 {
		font-size: 1.5em;
		font-weight: 600;
	}

	.share-auth p {
		color: var(--text-secondary);
		font-size: 0.95em;
	}

	.auth-link {
		display: inline-block;
		margin-top: var(--space-2);
		padding: var(--space-2) var(--space-6);
		background: var(--accent-blue);
		color: #fff;
		border-radius: var(--radius);
		font-weight: 500;
		text-decoration: none;
		transition: filter 0.15s ease;
	}

	.auth-link:hover {
		filter: brightness(1.1);
		text-decoration: none;
	}

	/* Error */
	.share-error {
		display: flex;
		flex-direction: column;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-10) 0;
		text-align: center;
	}

	.share-error h1 {
		font-size: 1.3em;
		font-weight: 600;
	}

	.share-error p {
		color: var(--text-secondary);
	}

	/* Item view */
	.share-item {
		display: flex;
		flex-direction: column;
		gap: var(--space-5);
	}

	.item-collection-badge {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		font-size: 0.82em;
		color: var(--text-muted);
		font-weight: 500;
		text-transform: uppercase;
		letter-spacing: 0.04em;
	}

	.collection-icon {
		font-size: 1.1em;
	}

	.item-ref {
		color: var(--text-muted);
		font-weight: 400;
	}

	.item-title {
		font-size: 2em;
		font-weight: 700;
		line-height: 1.25;
		letter-spacing: -0.02em;
	}

	.item-fields {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
	}

	.field-chip {
		display: inline-flex;
		align-items: center;
		gap: var(--space-1);
		padding: var(--space-1) var(--space-3);
		background: var(--bg-tertiary);
		border-radius: 999px;
		font-size: 0.82em;
	}

	.field-chip-label {
		color: var(--text-muted);
		font-weight: 500;
	}

	.field-chip-value {
		color: var(--text-primary);
	}

	.item-content {
		font-family: var(--font-content);
		font-size: 1em;
		line-height: 1.7;
		color: var(--text-primary);
	}

	/* Markdown content styles */
	.item-content :global(h1) {
		font-size: 1.6em;
		font-weight: 700;
		margin: 1.5em 0 0.5em;
		line-height: 1.3;
	}

	.item-content :global(h2) {
		font-size: 1.3em;
		font-weight: 600;
		margin: 1.4em 0 0.4em;
		line-height: 1.3;
	}

	.item-content :global(h3) {
		font-size: 1.1em;
		font-weight: 600;
		margin: 1.2em 0 0.3em;
	}

	.item-content :global(p) {
		margin: 0.8em 0;
	}

	.item-content :global(ul),
	.item-content :global(ol) {
		margin: 0.8em 0;
		padding-left: 1.5em;
	}

	.item-content :global(li) {
		margin: 0.3em 0;
	}

	.item-content :global(pre) {
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-4);
		overflow-x: auto;
		font-family: var(--font-mono);
		font-size: 0.88em;
		margin: 1em 0;
	}

	.item-content :global(code) {
		font-family: var(--font-mono);
		font-size: 0.9em;
		background: var(--bg-tertiary);
		padding: 0.15em 0.4em;
		border-radius: var(--radius-sm);
	}

	.item-content :global(pre code) {
		background: none;
		padding: 0;
	}

	.item-content :global(blockquote) {
		border-left: 3px solid var(--accent-blue);
		padding-left: var(--space-4);
		margin: 1em 0;
		color: var(--text-secondary);
	}

	.item-content :global(table) {
		width: 100%;
		border-collapse: collapse;
		margin: 1em 0;
	}

	.item-content :global(th),
	.item-content :global(td) {
		border: 1px solid var(--border);
		padding: var(--space-2) var(--space-3);
		text-align: left;
	}

	.item-content :global(th) {
		background: var(--bg-secondary);
		font-weight: 600;
	}

	.item-content :global(hr) {
		border: none;
		border-top: 1px solid var(--border);
		margin: 1.5em 0;
	}

	.item-content :global(img) {
		max-width: 100%;
		border-radius: var(--radius);
	}

	.item-content :global(a) {
		color: var(--accent-blue);
	}

	/* Collection view switcher toolbar */
	.collection-toolbar {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: var(--space-3);
		margin-bottom: var(--space-5);
	}

	.view-toggle {
		display: flex;
		border: 1px solid var(--border);
		border-radius: var(--radius);
		overflow: hidden;
		flex-shrink: 0;
	}

	.toggle-btn {
		display: inline-flex;
		align-items: center;
		gap: var(--space-1);
		background: var(--bg-secondary);
		border: none;
		padding: var(--space-1) var(--space-3);
		cursor: pointer;
		font-size: 0.85em;
		color: var(--text-secondary);
		line-height: 1;
	}

	.toggle-btn:not(:last-child) {
		border-right: 1px solid var(--border);
	}

	.toggle-btn.active {
		background: var(--bg-tertiary);
		color: var(--text-primary);
		font-weight: 500;
	}

	.toggle-btn:hover:not(.active) {
		background: var(--bg-hover, var(--bg-tertiary));
	}

	.toggle-icon {
		font-size: 1em;
	}

	.saved-views {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: var(--space-2);
	}

	.saved-views-label {
		font-size: 0.75em;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: var(--text-muted);
		font-weight: 500;
	}

	.saved-view-chip {
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: 999px;
		padding: var(--space-1) var(--space-3);
		cursor: pointer;
		font-size: 0.82em;
		color: var(--text-secondary);
		line-height: 1.2;
	}

	.saved-view-chip.active {
		background: var(--accent-blue);
		border-color: var(--accent-blue);
		color: #fff;
		font-weight: 500;
	}

	.saved-view-chip:hover:not(.active) {
		background: var(--bg-tertiary);
		color: var(--text-primary);
	}

	/* Widen the container for collection views — list/board/table need more
	   room than the 780px article column. */
	.share-container:has(.collection-toolbar) {
		max-width: 1100px;
	}

	@media (max-width: 640px) {
		.toggle-label {
			display: none;
		}

		.toggle-btn {
			padding: var(--space-1) var(--space-2);
		}
	}

	/* Footer */
	.share-footer {
		text-align: center;
		padding: var(--space-6) var(--space-5);
		border-top: 1px solid var(--border-subtle);
		font-size: 0.8em;
		color: var(--text-muted);
	}

	.share-footer a {
		color: var(--text-secondary);
		font-weight: 500;
	}

	.share-footer a:hover {
		color: var(--accent-blue);
	}

	.password-form {
		display: flex;
		flex-direction: column;
		align-items: center;
		gap: var(--space-3);
		width: 100%;
		max-width: 320px;
	}

	.password-input {
		width: 100%;
		padding: var(--space-2) var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--bg-secondary);
		color: var(--text-primary);
		font-size: 0.95em;
		text-align: center;
	}

	.password-input:focus {
		outline: none;
		border-color: var(--accent-blue);
	}

	.password-error {
		color: var(--accent-red, #e53e3e);
		font-size: 0.85em;
		margin: 0;
	}

	@media (max-width: 640px) {
		.share-container {
			padding: var(--space-5) var(--space-4);
		}

		.item-title {
			font-size: 1.5em;
		}
	}
</style>
