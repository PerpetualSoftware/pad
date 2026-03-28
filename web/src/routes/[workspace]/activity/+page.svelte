<script lang="ts">
	import { page } from '$app/state';
	import { onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { relativeTime } from '$lib/utils/markdown';
	import type { Activity, Collection } from '$lib/types';

	let wsSlug = $derived(page.params.workspace ?? '');

	// Data
	let activities = $state<Activity[]>([]);
	let collections = $state<Collection[]>([]);
	let loading = $state(true);
	let loadingMore = $state(false);
	let hasMore = $state(true);

	// Filters
	let filterAction = $state('');
	let filterSource = $state('');
	let filterCollection = $state('');

	const PAGE_SIZE = 30;

	// Reload when workspace or filters change
	$effect(() => {
		if (wsSlug) {
			// Access filter values to track them as dependencies
			filterAction;
			filterSource;
			loadActivities(wsSlug, true);
		}
	});

	// Load collections once
	$effect(() => {
		if (wsSlug) {
			loadCollections(wsSlug);
		}
	});

	onMount(() => {
		workspaceStore.setCurrent(wsSlug);
	});

	async function loadCollections(slug: string) {
		try {
			collections = await api.collections.list(slug);
		} catch {
			// allow partial render
		}
	}

	async function loadActivities(slug: string, reset = false) {
		if (reset) {
			loading = true;
			activities = [];
		} else {
			loadingMore = true;
		}

		try {
			const params: Record<string, string | number> = {
				limit: PAGE_SIZE,
				offset: reset ? 0 : activities.length
			};
			if (filterAction) params.action = filterAction;
			if (filterSource) params.source = filterSource;

			const result = await api.activity.list(slug, params);
			if (reset) {
				activities = result;
			} else {
				activities = [...activities, ...result];
			}
			hasMore = result.length >= PAGE_SIZE;
		} catch {
			// allow partial render
		} finally {
			loading = false;
			loadingMore = false;
		}
	}

	function loadMore() {
		if (!loadingMore && hasMore) {
			loadActivities(wsSlug, false);
		}
	}

	// Client-side collection filter using enriched top-level field or metadata fallback
	let filteredActivities = $derived.by(() => {
		if (!filterCollection) return activities;
		return activities.filter((a) => {
			if (a.collection_slug) return a.collection_slug === filterCollection;
			try {
				const meta = JSON.parse(a.metadata);
				return meta.collection_slug === filterCollection || meta.collection === filterCollection;
			} catch {
				return false;
			}
		});
	});

	// Group activities by date
	let groupedActivities = $derived.by(() => {
		const groups: { label: string; date: string; items: Activity[] }[] = [];
		const now = new Date();
		const today = new Date(now.getFullYear(), now.getMonth(), now.getDate());
		const yesterday = new Date(today.getTime() - 86400000);

		for (const activity of filteredActivities) {
			const actDate = new Date(activity.created_at);
			const actDay = new Date(actDate.getFullYear(), actDate.getMonth(), actDate.getDate());
			const dayKey = actDay.toISOString().slice(0, 10);

			let label: string;
			if (actDay.getTime() === today.getTime()) {
				label = 'Today';
			} else if (actDay.getTime() === yesterday.getTime()) {
				label = 'Yesterday';
			} else {
				label = actDay.toLocaleDateString('en-US', {
					weekday: 'long',
					month: 'long',
					day: 'numeric'
				});
			}

			const lastGroup = groups[groups.length - 1];
			if (lastGroup && lastGroup.date === dayKey) {
				lastGroup.items.push(activity);
			} else {
				groups.push({ label, date: dayKey, items: [activity] });
			}
		}
		return groups;
	});

	function activityVerb(action: string): string {
		switch (action) {
			case 'created':
				return 'Created';
			case 'updated':
				return 'Updated';
			case 'archived':
				return 'Archived';
			case 'restored':
				return 'Restored';
			case 'moved':
				return 'Moved';
			case 'field_changed':
				return 'Changed';
			default:
				return action;
		}
	}

	function activityIcon(action: string): string {
		if (action === 'created') return '+';
		if (action === 'updated' || action === 'field_changed') return '\u2022';
		if (action === 'archived') return '\u2212';
		if (action === 'restored') return '\u21ba';
		if (action === 'moved') return '\u2192';
		return '\u2022';
	}

	function actionColor(action: string): string {
		switch (action) {
			case 'created':
				return 'var(--accent-green)';
			case 'archived':
				return 'var(--text-muted)';
			case 'restored':
				return 'var(--accent-cyan)';
			case 'moved':
				return 'var(--accent-amber)';
			default:
				return 'var(--accent-blue)';
		}
	}

	function getSourceLabel(source: string, actor: string, actorName?: string): { label: string; kind: string } {
		if (actor === 'agent') return { label: 'agent', kind: 'agent' };
		if (actorName) return { label: actorName, kind: source === 'cli' ? 'cli' : 'user' };
		if (source === 'cli') return { label: 'cli', kind: 'cli' };
		return { label: 'web', kind: 'web' };
	}

	function borderClass(source: string, actor: string): string {
		if (source === 'cli' && actor === 'agent') return 'border-agent';
		if (source === 'cli') return 'border-cli';
		return '';
	}

	function parseMeta(metadata: string): Record<string, any> {
		try {
			return JSON.parse(metadata);
		} catch {
			return {};
		}
	}
</script>

<svelte:head>
	<title>Activity - {workspaceStore.current?.name ?? wsSlug} | Pad</title>
</svelte:head>

<div class="activity-page">
	<header class="page-header">
		<div class="page-header-left">
			<h1>Activity</h1>
			{#if !loading}
				<span class="entry-count"
					>{filteredActivities.length} entr{filteredActivities.length === 1
						? 'y'
						: 'ies'}</span
				>
			{/if}
		</div>
	</header>

	<!-- Filters -->
	<div class="filters-row">
		<div class="filter-group">
			<label class="filter-label" for="filter-action">Action</label>
			<select id="filter-action" class="filter-select" bind:value={filterAction}>
				<option value="">All actions</option>
				<option value="created">Created</option>
				<option value="updated">Updated</option>
				<option value="archived">Archived</option>
				<option value="restored">Restored</option>
				<option value="moved">Moved</option>
			</select>
		</div>

		<div class="filter-group">
			<label class="filter-label" for="filter-source">Source</label>
			<select id="filter-source" class="filter-select" bind:value={filterSource}>
				<option value="">All sources</option>
				<option value="web">Web</option>
				<option value="cli">CLI</option>
			</select>
		</div>

		<div class="filter-group">
			<label class="filter-label" for="filter-collection">Collection</label>
			<select id="filter-collection" class="filter-select" bind:value={filterCollection}>
				<option value="">All collections</option>
				{#each collections as coll (coll.id)}
					<option value={coll.slug}>{coll.icon} {coll.name}</option>
				{/each}
			</select>
		</div>

		{#if filterAction || filterSource || filterCollection}
			<button
				class="clear-filters"
				onclick={() => {
					filterAction = '';
					filterSource = '';
					filterCollection = '';
				}}
			>
				Clear filters
			</button>
		{/if}
	</div>

	<!-- Content -->
	{#if loading}
		<div class="skeleton-list">
			{#each Array(8) as _, i (i)}
				<div class="skeleton-row">
					<div class="skeleton-dot"></div>
					<div class="skeleton-line" style="width: {50 + Math.random() * 30}%;"></div>
					<div class="skeleton-line-sm"></div>
				</div>
			{/each}
		</div>
	{:else if filteredActivities.length === 0}
		<div class="empty-state">
			<div class="empty-icon">~</div>
			<p class="empty-title">No activity found</p>
			<p class="empty-desc">
				{#if filterAction || filterSource || filterCollection}
					Try changing or clearing the filters.
				{:else}
					Activity will appear here as items are created, updated, and managed.
				{/if}
			</p>
		</div>
	{:else}
		<div class="timeline">
			{#each groupedActivities as group (group.date)}
				<div class="date-group">
					<div class="date-header">
						<span class="date-label">{group.label}</span>
						<span class="date-line"></span>
					</div>
					<div class="date-entries">
						{#each group.items as activity (activity.id)}
							{@const meta = parseMeta(activity.metadata)}
							{@const itemTitle = activity.item_title || meta.item_title || meta.title}
							{@const itemSlug = activity.item_slug || meta.item_slug}
							{@const collSlug = activity.collection_slug || meta.collection_slug}
							{@const src = getSourceLabel(activity.source, activity.actor, activity.actor_name)}
							<div class="entry {borderClass(activity.source, activity.actor)}">
								<span
									class="entry-icon"
									style="color: {actionColor(activity.action)}"
								>
									{activityIcon(activity.action)}
								</span>
								<div class="entry-content">
									<div class="entry-main">
										<span class="entry-verb">{activityVerb(activity.action)}</span>
										{#if itemTitle && itemSlug && collSlug}
											<a
												href="/{wsSlug}/{collSlug}/{itemSlug}"
												class="entry-item-link">{itemTitle}</a
											>
										{:else if itemTitle}
											<span class="entry-item-name">{itemTitle}</span>
										{/if}
										{#if collSlug}
											<span class="entry-collection">{collSlug}</span>
										{/if}
									</div>
									{#if meta.changes}
										<div class="entry-detail">{meta.changes}</div>
									{/if}
								</div>
								<div class="entry-meta">
									<span class="actor-badge {src.kind}">{src.label}</span>
									<span
										class="entry-time"
										title={new Date(activity.created_at).toLocaleString()}
										>{relativeTime(activity.created_at)}</span
									>
								</div>
							</div>
						{/each}
					</div>
				</div>
			{/each}
		</div>

		{#if hasMore && !filterCollection}
			<div class="load-more-wrapper">
				<button class="load-more-btn" onclick={loadMore} disabled={loadingMore}>
					{#if loadingMore}
						Loading...
					{:else}
						Load more activity
					{/if}
				</button>
			</div>
		{/if}
	{/if}
</div>

<style>
	/* ── Page Layout ──────────────────────────────────────────────────── */
	.activity-page {
		max-width: var(--content-max-width);
		margin: 0 auto;
		padding: var(--space-8) var(--space-6);
	}

	/* ── Header ───────────────────────────────────────────────────────── */
	.page-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-4);
		margin-bottom: var(--space-6);
	}
	.page-header-left {
		display: flex;
		align-items: baseline;
		gap: var(--space-3);
	}
	.page-header h1 {
		font-size: 1.6em;
		font-weight: 700;
	}
	.entry-count {
		font-size: 0.9em;
		color: var(--text-muted);
	}

	/* ── Filters ──────────────────────────────────────────────────────── */
	.filters-row {
		display: flex;
		align-items: flex-end;
		gap: var(--space-4);
		margin-bottom: var(--space-6);
		flex-wrap: wrap;
	}
	.filter-group {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}
	.filter-label {
		font-size: 0.7em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.06em;
		color: var(--text-muted);
	}
	.filter-select {
		appearance: none;
		background: var(--bg-secondary);
		color: var(--text-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-2) var(--space-4) var(--space-2) var(--space-3);
		font-size: 0.85em;
		min-width: 140px;
		cursor: pointer;
		background-image: url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='10' height='6'%3E%3Cpath d='M0 0l5 6 5-6z' fill='%23666'/%3E%3C/svg%3E");
		background-repeat: no-repeat;
		background-position: right 10px center;
		transition: border-color 0.15s;
	}
	.filter-select:hover {
		border-color: var(--text-muted);
	}
	.filter-select:focus {
		outline: none;
		border-color: var(--accent-blue);
	}
	.clear-filters {
		background: none;
		border: none;
		color: var(--accent-blue);
		font-size: 0.85em;
		cursor: pointer;
		padding: var(--space-2) 0;
		white-space: nowrap;
	}
	.clear-filters:hover {
		text-decoration: underline;
	}

	/* ── Timeline ─────────────────────────────────────────────────────── */
	.timeline {
		display: flex;
		flex-direction: column;
		gap: var(--space-6);
	}

	/* ── Date Groups ──────────────────────────────────────────────────── */
	.date-header {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		margin-bottom: var(--space-3);
	}
	.date-label {
		font-size: 0.75em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.06em;
		color: var(--text-muted);
		white-space: nowrap;
	}
	.date-line {
		flex: 1;
		height: 1px;
		background: var(--border);
	}

	.date-entries {
		display: flex;
		flex-direction: column;
		gap: 2px;
	}

	/* ── Entry Row ────────────────────────────────────────────────────── */
	.entry {
		display: flex;
		align-items: flex-start;
		gap: var(--space-3);
		padding: var(--space-2) var(--space-3);
		border-radius: var(--radius-sm);
		border-left: 3px solid transparent;
		transition: background 0.15s;
	}
	.entry:hover {
		background: var(--bg-secondary);
	}
	.entry.border-agent {
		border-left-color: var(--accent-purple);
	}
	.entry.border-cli {
		border-left-color: var(--accent-blue);
	}

	.entry-icon {
		flex-shrink: 0;
		width: 18px;
		height: 18px;
		display: flex;
		align-items: center;
		justify-content: center;
		font-size: 1em;
		font-weight: 700;
		margin-top: 1px;
	}

	.entry-content {
		flex: 1;
		min-width: 0;
		display: flex;
		flex-direction: column;
		gap: 2px;
	}
	.entry-main {
		display: flex;
		align-items: baseline;
		gap: var(--space-2);
		flex-wrap: wrap;
		font-size: 0.875em;
	}
	.entry-verb {
		color: var(--text-secondary);
		white-space: nowrap;
	}
	.entry-item-link {
		font-weight: 600;
		color: var(--text-primary);
		text-decoration: none;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.entry-item-link:hover {
		color: var(--accent-blue);
		text-decoration: underline;
	}
	.entry-item-name {
		font-weight: 600;
		color: var(--text-primary);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.entry-collection {
		font-size: 0.8em;
		color: var(--text-muted);
		background: var(--bg-tertiary);
		padding: 1px 8px;
		border-radius: 10px;
		white-space: nowrap;
	}
	.entry-detail {
		font-size: 0.8em;
		color: var(--text-muted);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.entry-meta {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		flex-shrink: 0;
		margin-top: 1px;
	}

	.actor-badge {
		font-size: 0.7em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		padding: 1px 6px;
		border-radius: 3px;
		flex-shrink: 0;
	}
	.actor-badge.agent {
		background: color-mix(in srgb, var(--accent-purple) 15%, transparent);
		color: var(--accent-purple);
	}
	.actor-badge.cli {
		background: color-mix(in srgb, var(--accent-blue) 15%, transparent);
		color: var(--accent-blue);
	}
	.actor-badge.web {
		background: var(--bg-tertiary);
		color: var(--text-muted);
	}
	.actor-badge.user {
		background: color-mix(in srgb, var(--accent-green) 15%, transparent);
		color: var(--accent-green);
		text-transform: none;
		letter-spacing: normal;
	}

	.entry-time {
		font-size: 0.8em;
		color: var(--text-muted);
		white-space: nowrap;
	}

	/* ── Empty State ──────────────────────────────────────────────────── */
	.empty-state {
		text-align: center;
		padding: var(--space-10) var(--space-4);
		color: var(--text-muted);
	}
	.empty-icon {
		font-size: 2em;
		margin-bottom: var(--space-3);
		opacity: 0.5;
	}
	.empty-title {
		font-size: 1.1em;
		font-weight: 600;
		color: var(--text-secondary);
		margin-bottom: var(--space-2);
	}
	.empty-desc {
		font-size: 0.9em;
	}

	/* ── Load More ────────────────────────────────────────────────────── */
	.load-more-wrapper {
		display: flex;
		justify-content: center;
		padding: var(--space-6) 0;
	}
	.load-more-btn {
		background: var(--bg-secondary);
		color: var(--text-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-2) var(--space-6);
		font-size: 0.85em;
		font-weight: 600;
		cursor: pointer;
		transition: background 0.15s, border-color 0.15s, color 0.15s;
	}
	.load-more-btn:hover:not(:disabled) {
		border-color: var(--text-muted);
		color: var(--text-primary);
		background: var(--bg-hover);
	}
	.load-more-btn:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}

	/* ── Skeleton ─────────────────────────────────────────────────────── */
	.skeleton-list {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
		padding-top: var(--space-4);
	}
	.skeleton-row {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-2) var(--space-3);
	}
	.skeleton-dot {
		width: 18px;
		height: 18px;
		border-radius: 50%;
		background: var(--bg-tertiary);
		flex-shrink: 0;
		animation: skeleton-pulse 1.5s ease-in-out infinite;
	}
	.skeleton-line {
		height: 14px;
		border-radius: var(--radius-sm);
		background: var(--bg-tertiary);
		animation: skeleton-pulse 1.5s ease-in-out infinite;
	}
	.skeleton-line-sm {
		width: 60px;
		height: 12px;
		border-radius: var(--radius-sm);
		background: var(--bg-tertiary);
		margin-left: auto;
		animation: skeleton-pulse 1.5s ease-in-out infinite;
	}
	@keyframes skeleton-pulse {
		0%,
		100% {
			opacity: 0.5;
		}
		50% {
			opacity: 1;
		}
	}

	/* ── Responsive ───────────────────────────────────────────────────── */
	@media (max-width: 768px) {
		.filters-row {
			flex-direction: column;
			align-items: stretch;
			gap: var(--space-3);
		}
		.filter-select {
			min-width: unset;
			width: 100%;
		}
		.entry {
			flex-wrap: wrap;
		}
		.entry-meta {
			width: 100%;
			padding-left: 30px;
		}
	}
</style>
