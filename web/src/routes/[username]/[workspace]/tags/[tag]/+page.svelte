<script lang="ts">
	import { page } from '$app/state';
	import { api } from '$lib/api/client';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { createScrollRestoration } from '$lib/scroll/restore.svelte';
	import ItemCard from '$lib/components/collections/ItemCard.svelte';
	import type { Item, Collection } from '$lib/types';

	let wsSlug = $derived(page.params.workspace ?? '');
	// Route params arrive URL-decoded, so this is the human-readable tag.
	let tag = $derived(page.params.tag ?? '');

	let fetchedItems = $state<Item[]>([]);
	let collections = $state<Collection[]>([]);
	let loading = $state(true);
	let loadSeq = 0;

	const scrollRestoration = createScrollRestoration({
		ready: () => !loading,
		persistKey: () => (wsSlug ? `pad-last-scroll-${wsSlug}-${page.url.pathname}` : null)
	});
	export const snapshot = scrollRestoration.snapshot;

	// Re-fetch when the workspace or the tag changes. Pure data-fetch effect
	// keyed on (wsSlug, tag) — kept free of unrelated route logic per the
	// Svelte 5 effect-splitting convention.
	$effect(() => {
		const ws = wsSlug;
		const t = tag;
		if (ws && t) loadTagged(ws, t);
	});

	async function loadTagged(ws: string, t: string) {
		loading = true;
		const seq = ++loadSeq;
		try {
			const [items, colls] = await Promise.all([
				api.items.list(ws, { tag: t }),
				api.collections.list(ws)
			]);
			if (seq !== loadSeq) return;
			fetchedItems = items;
			collections = colls;
		} catch {
			if (seq !== loadSeq) return;
			fetchedItems = [];
		} finally {
			if (seq === loadSeq) loading = false;
		}
	}

	function getCollection(collectionId: string): Collection | undefined {
		return collections.find((c) => c.id === collectionId);
	}

	// A restricted member can see an item via an item-level grant without being
	// able to list its collection (collections.list is filtered by visible
	// collections). Synthesize a minimal collection from the metadata already
	// embedded on the item so those rows still render — dropping them would
	// make the header count disagree with the visible list. The empty schema
	// means ItemCard just omits status/priority for these rows. Per Codex
	// PR #660 round 3. Sorted last (no real sort_order available).
	function syntheticCollection(item: Item): Collection {
		return {
			id: item.collection_id,
			workspace_id: item.workspace_id,
			name: item.collection_name ?? 'Items',
			slug: item.collection_slug ?? '',
			icon: item.collection_icon ?? '📁',
			description: '',
			schema: '{"fields":[]}',
			settings: '{}',
			sort_order: Number.MAX_SAFE_INTEGER,
			is_default: false,
			is_system: false,
			created_at: item.created_at,
			updated_at: item.updated_at,
			prefix: item.collection_prefix ?? ''
		};
	}

	// Aggregate tagged items the way collections are viewed: grouped by
	// collection (the shared axis across heterogeneous status enums), each
	// group ordered by the collection's sort_order.
	let groupedItems = $derived.by(() => {
		const map = new Map<string, { collection: Collection; items: Item[] }>();
		for (const item of fetchedItems) {
			const key = item.collection_id;
			let group = map.get(key);
			if (!group) {
				group = { collection: getCollection(key) ?? syntheticCollection(item), items: [] };
				map.set(key, group);
			}
			group.items.push(item);
		}
		const groups = [...map.values()];
		groups.sort((a, b) => a.collection.sort_order - b.collection.sort_order);
		return groups;
	});
</script>

<svelte:head>
	<title>#{tag} - {workspaceStore.current?.name ?? wsSlug} | Pad</title>
</svelte:head>

<div class="tag-page">
	<div class="page-header">
		<div class="page-header-left">
			<a class="back-link" href="/{page.params.username}/{wsSlug}/tags">🏷 Tags</a>
			<span class="crumb-sep">/</span>
			<h1>{tag}</h1>
			<span class="item-count">{fetchedItems.length} item{fetchedItems.length !== 1 ? 's' : ''}</span>
		</div>
	</div>

	{#if loading}
		<div class="loading-state">
			<div class="skeleton-list">
				{#each Array(3) as _, i (i)}
					<div class="skeleton-card"></div>
				{/each}
			</div>
		</div>
	{:else if fetchedItems.length === 0}
		<div class="empty-state">
			<div class="empty-icon">🏷</div>
			<h2>No items tagged “{tag}”</h2>
			<p>Add this tag to an item from its detail page to group it here.</p>
		</div>
	{:else}
		<div class="tag-list">
			{#each groupedItems as group (group.collection.id)}
				<div class="collection-group">
					<div class="group-header">
						<span class="group-icon">{group.collection.icon || '📁'}</span>
						<span class="group-name">{group.collection.name}</span>
						<span class="group-count">{group.items.length}</span>
					</div>
					<div class="group-items">
						{#each group.items as item (item.id)}
							<ItemCard {item} collection={group.collection} showCollection={false} />
						{/each}
					</div>
				</div>
			{/each}
		</div>
	{/if}
</div>

<style>
	.tag-page {
		max-width: var(--content-max-width);
		margin: 0 auto;
		padding: var(--space-8) var(--space-6);
	}

	.page-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-4);
		margin-bottom: var(--space-6);
		flex-wrap: wrap;
	}

	.page-header-left {
		display: flex;
		align-items: baseline;
		gap: var(--space-3);
		flex-wrap: wrap;
	}

	.back-link {
		font-size: 0.95em;
		font-weight: 600;
		color: var(--text-secondary);
		text-decoration: none;
	}

	.back-link:hover {
		color: var(--text-primary);
	}

	.crumb-sep {
		color: var(--text-muted);
	}

	.page-header h1 {
		font-size: 1.6em;
		font-weight: 700;
		color: var(--text-primary);
		margin: 0;
		word-break: break-word;
	}

	.item-count {
		font-size: 0.85em;
		color: var(--text-muted);
	}

	.loading-state {
		padding: var(--space-4) 0;
	}

	.skeleton-list {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}

	.skeleton-card {
		height: 72px;
		background: var(--bg-secondary);
		border-radius: var(--radius);
		animation: pulse 1.5s ease-in-out infinite;
	}

	@keyframes pulse {
		0%,
		100% {
			opacity: 0.4;
		}
		50% {
			opacity: 0.7;
		}
	}

	.empty-state {
		text-align: center;
		padding: var(--space-12) var(--space-6);
		color: var(--text-muted);
	}

	.empty-icon {
		font-size: 3em;
		margin-bottom: var(--space-4);
		opacity: 0.4;
	}

	.empty-state h2 {
		font-size: 1.1em;
		font-weight: 600;
		color: var(--text-secondary);
		margin: 0 0 var(--space-2);
	}

	.empty-state p {
		font-size: 0.9em;
		line-height: 1.5;
		max-width: 400px;
		margin: 0 auto;
	}

	.tag-list {
		display: flex;
		flex-direction: column;
		gap: var(--space-6);
	}

	.collection-group {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}

	.group-header {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-2) 0;
	}

	.group-icon {
		font-size: 0.9em;
	}

	.group-name {
		font-size: 0.85em;
		font-weight: 600;
		color: var(--text-secondary);
		text-transform: uppercase;
		letter-spacing: 0.03em;
	}

	.group-count {
		font-size: 0.75em;
		color: var(--text-muted);
		background: var(--bg-tertiary);
		padding: 1px 6px;
		border-radius: 10px;
	}

	.group-items {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
</style>
