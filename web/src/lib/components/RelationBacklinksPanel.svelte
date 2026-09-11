<script lang="ts">
	import { SvelteMap } from 'svelte/reactivity';
	import { api } from '$lib/api/client';
	import type { RelationBacklink } from '$lib/types';

	interface Props {
		wsSlug: string;
		username: string;
		itemSlug: string;
		itemId: string;
	}

	let { wsSlug, username, itemSlug, itemId }: Props = $props();

	const PAGE_LIMIT = 50;

	let rows = $state<RelationBacklink[]>([]);
	/**
	 * The server's count under THIS viewer's visibility — not the true one.
	 * "Referenced by 3" means "by 3 you can see", which is the ratified
	 * decision: a true count would tell the viewer that items they cannot open
	 * exist. Rendered from `total` rather than `rows.length` so the header is
	 * right when the list is paginated.
	 */
	let total = $state(0);
	let loading = $state(true);
	let loadingMore = $state(false);
	let error = $state('');
	let hasMore = $state(false);

	$effect(() => {
		void itemSlug;
		void itemId;
		void wsSlug;
		loadFirstPage();
	});

	async function loadFirstPage() {
		// Capture the request identity BEFORE the await. ItemDetail reuses this
		// panel across a no-{#key} item switch — the props just change — so a
		// slower load for item A must not overwrite B's rows or push A's count
		// into the parent's badge. Same fence BacklinksPanel carries, for the
		// same reason and the same bug class (PLAN-2105 / TASK-2112).
		const reqSlug = itemSlug;
		const reqWs = wsSlug;
		loading = true;
		error = '';
		try {
			const page = await api.items.relationBacklinks(reqWs, reqSlug, { limit: PAGE_LIMIT });
			if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
			rows = page.relation_backlinks;
			total = page.total;
			hasMore = page.relation_backlinks.length === PAGE_LIMIT && page.total > page.relation_backlinks.length;
		} catch (err) {
			if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
			error = err instanceof Error ? err.message : 'Failed to load references';
			rows = [];
			total = 0;
			hasMore = false;
		} finally {
			if (reqSlug === itemSlug && reqWs === wsSlug) loading = false;
		}
	}

	async function loadMore() {
		const reqSlug = itemSlug;
		const reqWs = wsSlug;
		loadingMore = true;
		try {
			const page = await api.items.relationBacklinks(reqWs, reqSlug, {
				limit: PAGE_LIMIT,
				offset: rows.length
			});
			if (reqSlug !== itemSlug || reqWs !== wsSlug) return;
			rows = [...rows, ...page.relation_backlinks];
			total = page.total;
			hasMore = rows.length < page.total;
		} catch {
			// A failed "show more" leaves what is already displayed alone —
			// dropping loaded rows because a later page failed would be worse
			// than stopping.
			if (reqSlug === itemSlug && reqWs === wsSlug) hasMore = false;
		} finally {
			if (reqSlug === itemSlug && reqWs === wsSlug) loadingMore = false;
		}
	}

	/**
	 * Grouped by the FIELD that points here, not by collection.
	 *
	 * That is the difference from "Mentioned in": a relation edge is typed, so
	 * "three items reference this through `owner`" is the useful reading, and
	 * an item referenced through two different fields genuinely belongs under
	 * both. SvelteMap's insertion order preserves the server's ordering.
	 */
	type Group = { key: string; label: string; rows: RelationBacklink[] };

	let groups = $derived.by<Group[]>(() => {
		const map = new SvelteMap<string, Group>();
		for (const row of rows) {
			const key = row.field_key;
			let group = map.get(key);
			if (!group) {
				group = { key, label: row.field_label || row.field_key, rows: [] };
				map.set(key, group);
			}
			group.rows.push(row);
		}
		return Array.from(map.values());
	});

	function rowHref(row: RelationBacklink): string {
		// A row with no ref is a legacy item with no item_number; its slug is
		// not carried here, so it links to the collection rather than 404ing
		// on an empty ref segment.
		if (!row.source_ref) return `/${username}/${wsSlug}/${row.collection_slug}`;
		return `/${username}/${wsSlug}/${row.collection_slug}/${row.source_ref}`;
	}

	/**
	 * One source can point at the target through TWO fields, so
	 * `source_item_id` alone is not unique. Svelte 5 rejects duplicate keys in
	 * dev and silently reuses DOM in prod, which would render one of the two.
	 */
	function rowKey(row: RelationBacklink, index: number): string {
		return `${row.source_item_id}|${row.field_key}|${index}`;
	}

	// Collapse entirely on zero — most items are referenced by nothing and an
	// empty header is noise. A failed fetch still shows a one-liner, because
	// silently dropping it is worse than a small note.
	let shouldRender = $derived(loading || error !== '' || rows.length > 0);
</script>

{#if shouldRender}
	<div class="relation-backlinks-panel">
		<div class="panel-header">
			<h3>Referenced by</h3>
			{#if !loading && !error && total > 0}
				<span class="count">{total}</span>
			{/if}
		</div>

		{#if loading}
			<div class="loading">Loading references…</div>
		{:else if error}
			<div class="error">{error}</div>
		{:else}
			{#each groups as group (group.key)}
				<div class="field-group">
					<div class="field-label">{group.label}</div>
					<ul class="rows">
						{#each group.rows as row, i (rowKey(row, i))}
							<li class="row">
								<a href={rowHref(row)} class="row-link">
									{#if row.source_ref}
										<span class="row-ref">{row.source_ref}</span>
									{/if}
									<span class="row-title">{row.source_title}</span>
								</a>
							</li>
						{/each}
					</ul>
				</div>
			{/each}

			{#if hasMore}
				<div class="more">
					<button type="button" class="more-btn" disabled={loadingMore} onclick={loadMore}>
						{loadingMore ? 'Loading…' : 'Show more'}
					</button>
				</div>
			{/if}
		{/if}
	</div>
{/if}

<style>
	.relation-backlinks-panel {
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
	}

	.count {
		font-size: 0.85em;
		color: var(--text-muted);
	}

	.loading,
	.error {
		font-size: 0.85em;
		color: var(--text-muted);
	}

	.field-group {
		margin-bottom: var(--space-3);
	}

	.field-label {
		font-size: 0.8em;
		color: var(--text-muted);
		text-transform: uppercase;
		letter-spacing: 0.04em;
		margin-bottom: var(--space-1);
	}

	.rows {
		list-style: none;
		margin: 0;
		padding: 0;
	}

	.row {
		padding: var(--space-1) 0;
	}

	.row-link {
		display: inline-flex;
		align-items: baseline;
		gap: var(--space-2);
		text-decoration: none;
		color: inherit;
	}

	.row-link:hover .row-title {
		text-decoration: underline;
	}

	.row-ref {
		font-family: var(--font-mono, monospace);
		font-size: 0.8em;
		color: var(--text-muted);
	}

	.more-btn {
		background: none;
		border: 1px solid var(--border);
		border-radius: var(--radius-sm, 4px);
		padding: var(--space-1) var(--space-2);
		font-size: 0.85em;
		cursor: pointer;
		color: inherit;
	}

	.more-btn:disabled {
		opacity: 0.6;
		cursor: default;
	}
</style>
