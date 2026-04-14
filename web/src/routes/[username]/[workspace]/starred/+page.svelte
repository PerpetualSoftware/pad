<script lang="ts">
	import { page } from '$app/state';
	import { onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { starredStore } from '$lib/stores/starred.svelte';
	import ItemCard from '$lib/components/collections/ItemCard.svelte';
	import type { Item, Collection } from '$lib/types';

	let wsSlug = $derived(page.params.workspace ?? '');
	let username = $derived(page.params.username ?? '');

	let fetchedItems = $state<Item[]>([]);
	let collections = $state<Collection[]>([]);
	let loading = $state(true);
	let includeTerminal = $state(false);
	let loadSeq = 0;

	// Filter fetched items by current starred state so unstars are reflected immediately
	let items = $derived(fetchedItems.filter(i => starredStore.isStarred(i.id)));

	$effect(() => {
		if (wsSlug) {
			// Re-fetch when wsSlug or terminal filter changes
			const _terminal = includeTerminal;
			loadStarred(wsSlug);
		}
	});

	onMount(() => {
		workspaceStore.setCurrent(wsSlug);
	});

	async function loadStarred(slug: string) {
		loading = true;
		const seq = ++loadSeq;
		try {
			const [starredItems, colls] = await Promise.all([
				api.items.starred(slug, { include_terminal: includeTerminal }),
				api.collections.list(slug)
			]);
			if (seq !== loadSeq) return;
			fetchedItems = starredItems;
			collections = colls;
		} catch {
			if (seq !== loadSeq) return;
		} finally {
			if (seq === loadSeq) loading = false;
		}
	}

	function getCollection(collectionId: string): Collection | undefined {
		return collections.find(c => c.id === collectionId);
	}

	// Group items by collection
	let groupedItems = $derived.by(() => {
		const groups: { collection: Collection; items: Item[] }[] = [];
		const map = new Map<string, Item[]>();

		for (const item of items) {
			const key = item.collection_id;
			if (!map.has(key)) {
				map.set(key, []);
			}
			map.get(key)!.push(item);
		}

		for (const [collId, collItems] of map) {
			const coll = getCollection(collId);
			if (coll) {
				groups.push({ collection: coll, items: collItems });
			}
		}

		// Sort groups by collection sort_order
		groups.sort((a, b) => a.collection.sort_order - b.collection.sort_order);
		return groups;
	});

	// No explicit handleUnstar needed — items is derived from starredStore.isStarred,
	// so unstarring via ItemCard's toggle automatically removes the item from the list.
</script>

<svelte:head>
	<title>Starred - {workspaceStore.current?.name ?? wsSlug} | Pad</title>
</svelte:head>

<div class="starred-page">
	<div class="page-header">
		<div class="header-top">
			<h1>⭐ Starred</h1>
			<span class="item-count">{items.length} item{items.length !== 1 ? 's' : ''}</span>
		</div>
		<div class="header-controls">
			<label class="terminal-toggle">
				<input type="checkbox" bind:checked={includeTerminal} />
				Show completed
			</label>
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
	{:else if items.length === 0}
		<div class="empty-state">
			<div class="empty-icon">☆</div>
			<h2>No starred items</h2>
			<p>Star items to keep track of things that matter to you. Click the ☆ on any item in a list or detail view to star it.</p>
		</div>
	{:else}
		<div class="starred-list">
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
	.starred-page {
		max-width: 800px;
		margin: 0 auto;
		padding: var(--space-6) var(--space-6) var(--space-12);
	}

	.page-header {
		margin-bottom: var(--space-6);
	}

	.header-top {
		display: flex;
		align-items: baseline;
		gap: var(--space-3);
		margin-bottom: var(--space-3);
	}

	.page-header h1 {
		font-size: 1.5em;
		font-weight: 700;
		color: var(--text-primary);
		margin: 0;
	}

	.item-count {
		font-size: 0.85em;
		color: var(--text-muted);
	}

	.header-controls {
		display: flex;
		align-items: center;
		gap: var(--space-3);
	}

	.terminal-toggle {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		font-size: 0.85em;
		color: var(--text-secondary);
		cursor: pointer;
	}

	.terminal-toggle input {
		cursor: pointer;
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
		0%, 100% { opacity: 0.4; }
		50% { opacity: 0.7; }
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

	.starred-list {
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
