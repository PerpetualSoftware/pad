<script lang="ts">
	import { SvelteMap } from 'svelte/reactivity';
	import { api } from '$lib/api/client';
	import type { Backlink } from '$lib/types';
	import { relativeTime } from '$lib/utils/markdown';

	interface Props {
		wsSlug: string;
		username: string;
		itemSlug: string;
		itemId: string;
		onCountChange?: (n: number) => void;
	}

	let { wsSlug, username, itemSlug, itemId, onCountChange }: Props = $props();

	const PAGE_LIMIT = 50;

	let backlinks = $state<Backlink[]>([]);
	let loading = $state(true);
	let loadingMore = $state(false);
	let error = $state('');
	let hasMore = $state(false);

	/**
	 * Reload when the queried item changes. We depend on BOTH itemSlug and
	 * itemId so the panel refetches when the parent page swaps items
	 * without unmount/remount — itemSlug alone could collide across
	 * workspaces, and itemId alone wouldn't catch a slug-only rename of
	 * the same id. Touching both via `void` keeps the effect dependent
	 * regardless of whether their values are actually used downstream.
	 */
	$effect(() => {
		void itemSlug;
		void itemId;
		void wsSlug;
		loadFirstPage();
	});

	async function loadFirstPage() {
		loading = true;
		error = '';
		try {
			const rows = await api.items.backlinks(wsSlug, itemSlug, { limit: PAGE_LIMIT });
			backlinks = rows;
			hasMore = rows.length === PAGE_LIMIT;
			onCountChange?.(rows.length);
		} catch (err) {
			error = err instanceof Error ? err.message : 'Failed to load backlinks';
			backlinks = [];
			hasMore = false;
			// Empty contract: signal zero so the badge stays hidden on error.
			onCountChange?.(0);
		} finally {
			loading = false;
		}
	}

	async function loadMore() {
		if (loadingMore || !hasMore) return;
		loadingMore = true;
		try {
			const rows = await api.items.backlinks(wsSlug, itemSlug, {
				limit: PAGE_LIMIT,
				offset: backlinks.length
			});
			backlinks = [...backlinks, ...rows];
			hasMore = rows.length === PAGE_LIMIT;
			onCountChange?.(backlinks.length);
		} catch (err) {
			error = err instanceof Error ? err.message : 'Failed to load more backlinks';
		} finally {
			loadingMore = false;
		}
	}

	/**
	 * Group rows by source_collection_slug, scoped by source_workspace_slug
	 * so cross-workspace rows don't merge with same-workspace rows that
	 * happen to use the same collection slug. The grouping key embeds the
	 * workspace (defaulting to the current wsSlug for same-ws rows) so
	 * iteration order follows the response order — the API already sorts
	 * by updated_at DESC, so SvelteMap's insertion-order iteration gives
	 * us "most-recently-touched collection first" for free.
	 */
	type Group = {
		key: string;
		icon: string;
		slug: string;
		workspaceSlug: string | null; // null = same workspace as wsSlug
		rows: Backlink[];
	};

	let groups = $derived.by<Group[]>(() => {
		const map = new SvelteMap<string, Group>();
		for (const bl of backlinks) {
			const xws = bl.source_workspace_slug ?? null;
			const key = `${xws ?? wsSlug}::${bl.source_collection_slug}`;
			let group = map.get(key);
			if (!group) {
				group = {
					key,
					icon: bl.source_collection_icon,
					slug: bl.source_collection_slug,
					workspaceSlug: xws,
					rows: []
				};
				map.set(key, group);
			}
			group.rows.push(bl);
		}
		return Array.from(map.values());
	});

	function capitalize(s: string): string {
		if (!s) return s;
		return s.charAt(0).toUpperCase() + s.slice(1);
	}

	function rowHref(bl: Backlink): string {
		const ws = bl.source_workspace_slug ?? wsSlug;
		return `/${username}/${ws}/${bl.source_collection_slug}/${bl.source_ref}`;
	}

	// Render nothing while we don't yet know there are backlinks. Once
	// loaded, collapse entirely on zero rows — most items have no inbound
	// links and an empty "Mentioned in" header is just noise. The error
	// case still shows a muted one-liner because silently dropping a
	// failed fetch is worse than a tiny note.
	let shouldRender = $derived(loading || error !== '' || backlinks.length > 0);
</script>

{#if shouldRender}
	<div class="backlinks-panel">
		<div class="panel-header">
			<h3>Mentioned in</h3>
			{#if !loading && !error && backlinks.length > 0}
				<span class="count">{backlinks.length}{hasMore ? '+' : ''}</span>
			{/if}
		</div>

		{#if loading}
			<div class="loading">
				<span class="spinner" aria-hidden="true"></span>
				<span>Loading…</span>
			</div>
		{:else if error}
			<div class="error">Couldn't load backlinks.</div>
		{:else}
			{#each groups as group (group.key)}
				<div class="group">
					<div class="group-header">
						<span class="group-icon" aria-hidden="true">{group.icon}</span>
						<span class="group-label">{capitalize(group.slug)}</span>
						{#if group.workspaceSlug}
							<span class="group-ws" title="From workspace {group.workspaceSlug}">
								→ {group.workspaceSlug}
							</span>
						{/if}
					</div>
					<ul class="rows">
						{#each group.rows as bl (bl.source_item_id)}
							<li class="row">
								<div class="row-head">
									<a href={rowHref(bl)} class="row-link">
										<span class="row-ref">{bl.source_ref}</span>
										<span class="row-title">{bl.source_title}</span>
									</a>
									{#if bl.display_text}
										<span class="display-tag" title="Rendered as">
											(displayed as: {bl.display_text})
										</span>
									{/if}
									<span class="row-time" title={bl.updated_at}>
										{relativeTime(bl.updated_at)}
									</span>
								</div>
								{#if bl.snippet}
									<div class="snippet">{bl.snippet}</div>
								{/if}
							</li>
						{/each}
					</ul>
				</div>
			{/each}

			{#if hasMore}
				<div class="more">
					<button
						type="button"
						class="more-btn"
						disabled={loadingMore}
						onclick={loadMore}
					>
						{loadingMore ? 'Loading…' : 'Show older'}
					</button>
				</div>
			{/if}
		{/if}
	</div>
{/if}

<style>
	.backlinks-panel {
		padding: var(--space-4) 0;
		border-top: 1px solid var(--border);
	}

	.panel-header {
		display: flex;
		align-items: baseline;
		gap: var(--space-2);
		margin-bottom: var(--space-3);
	}

	.panel-header h3 {
		margin: 0;
		font-size: 0.95em;
		font-weight: 600;
		color: var(--text-primary);
	}

	.count {
		font-size: 0.8em;
		color: var(--text-muted);
		font-weight: 400;
	}

	.group {
		margin-top: var(--space-3);
	}

	.group:first-of-type {
		margin-top: 0;
	}

	.group-header {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		font-size: 0.72em;
		font-weight: 500;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
		margin-bottom: var(--space-2);
	}

	.group-icon {
		font-size: 1.1em;
		line-height: 1;
	}

	.group-ws {
		text-transform: none;
		letter-spacing: 0;
		font-weight: 400;
		font-size: 0.95em;
		color: var(--text-muted);
		opacity: 0.7;
	}

	.rows {
		list-style: none;
		padding: 0;
		margin: 0;
	}

	.row {
		padding: var(--space-2) var(--space-2);
		border-bottom: 1px solid var(--border);
		min-width: 0;
	}

	.row:last-child {
		border-bottom: none;
	}

	.row-head {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		min-width: 0;
	}

	.row-link {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		flex: 1;
		min-width: 0;
		text-decoration: none;
		color: inherit;
	}

	.row-link:hover .row-title {
		text-decoration: underline;
	}

	.row-ref {
		font-family: var(--font-mono);
		font-size: 0.78em;
		color: var(--text-muted);
		white-space: nowrap;
		flex-shrink: 0;
	}

	.row-title {
		font-size: 0.88em;
		color: var(--text-primary);
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.display-tag {
		font-size: 0.75em;
		color: var(--text-muted);
		font-style: italic;
		flex-shrink: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		max-width: 12em;
	}

	.row-time {
		font-size: 0.75em;
		color: var(--text-muted);
		white-space: nowrap;
		flex-shrink: 0;
		margin-left: auto;
	}

	.snippet {
		margin-top: 2px;
		padding-left: 0;
		font-size: 0.82em;
		color: var(--text-muted);
		line-height: 1.4;
		overflow: hidden;
		text-overflow: ellipsis;
		display: -webkit-box;
		-webkit-line-clamp: 2;
		line-clamp: 2;
		-webkit-box-orient: vertical;
		word-break: break-word;
	}

	.loading {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-2) 0;
		color: var(--text-muted);
		font-size: 0.85em;
	}

	.spinner {
		width: 12px;
		height: 12px;
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

	.error {
		padding: var(--space-2) 0;
		color: var(--text-muted);
		font-size: 0.85em;
		font-style: italic;
	}

	.more {
		margin-top: var(--space-3);
		display: flex;
		justify-content: center;
	}

	.more-btn {
		background: none;
		border: 1px solid var(--border);
		color: var(--text-secondary, var(--text-muted));
		padding: var(--space-1) var(--space-3);
		border-radius: var(--radius-sm);
		font-size: 0.8em;
		cursor: pointer;
		transition: background 0.1s, color 0.1s;
	}

	.more-btn:hover:not(:disabled) {
		background: var(--bg-hover);
		color: var(--text-primary);
	}

	.more-btn:disabled {
		opacity: 0.6;
		cursor: default;
	}
</style>
