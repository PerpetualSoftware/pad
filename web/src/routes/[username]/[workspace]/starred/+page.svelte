<script lang="ts">
	import { page } from '$app/state';
	import { onMount, untrack } from 'svelte';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { starredStore } from '$lib/stores/starred.svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { localIndex } from '$lib/stores/localIndex.svelte';
	import { enterWorkspaceIndex } from '$lib/stores/workspaceIndexEntry';
	import { isOpen } from '$lib/items/localLists';
	import { createScrollRestoration } from '$lib/scroll/restore.svelte';
	import ItemCard from '$lib/components/collections/ItemCard.svelte';
	import PageHeader from '$lib/components/common/PageHeader.svelte';
	import EmptyState from '$lib/components/common/EmptyState.svelte';
	import ContentError from '$lib/components/common/ContentError.svelte';
	import { loadFailure } from '$lib/api/loadFailure';
	import type { Item, Collection } from '$lib/types';

	let wsSlug = $derived(page.params.workspace ?? '');
	let username = $derived(page.params.username ?? '');

	// Built from what the workspace already holds (TASK-2231): the starred
	// store's ids, in star order, and the local index's rows for them. The page
	// used to fetch every starred item with its full body, plus its own copy of
	// the collection list, on every visit and every toggle of "Show completed";
	// it now makes no request of its own.
	let includeTerminal = $state(false);
	let starsReady = $derived(!!wsSlug && starredStore.loaded && starredStore.workspace === wsSlug);
	let indexState = $derived(wsSlug ? localIndex.bootstrapStateFor(wsSlug) : 'cold');
	// The collection list the workspace layout keeps, gated on being THIS
	// workspace's: mid-switch the array still holds the previous one's.
	let collectionsReady = $derived(!!wsSlug && collectionStore.collectionsAreFreshFor(wsSlug));
	let collections = $derived<Collection[]>(collectionsReady ? collectionStore.collections : []);
	// A revoked caller's index is RESET, not failed (see the tag page).
	let accessRevoked = $derived(!!wsSlug && localIndex.accessRevokedFor(wsSlug));
	let collectionsError = $state<unknown>(null);
	// TASK-2203: a failed load is an error with a retry, never "No starred items".
	let loadError = $derived<unknown>(
		accessRevoked
			? { code: 'forbidden' }
			: starsReady && starredStore.error
				? starredStore.error
				: indexState === 'error'
					? new Error()
					: collectionsError
	);
	// All three, because the completed filter judges each item by its
	// collection's done field: before the list lands it would use the defaults.
	let loading = $derived(!loadError && (!starsReady || indexState !== 'ready' || !collectionsReady));

	// Scroll position restoration (BUG-1425).
	const scrollRestoration = createScrollRestoration({
		ready: () => !loading,
		persistKey: () =>
			wsSlug ? `pad-last-scroll-${wsSlug}-${page.url.pathname}` : null,
	});
	export const snapshot = scrollRestoration.snapshot;

	// Bring the index up and catch it up, the shared way (BUG-3181). Reads
	// `authStore.userId` on purpose so it re-runs for a new identity, and
	// captures the epoch synchronously for the helper's fence. The call itself
	// is UNTRACKED: `localIndex.bootstrap` reads and writes the workspace's
	// bootstrap state before its first await, and tracked, each write re-ran
	// this effect (BUG-3192, the same trap on the item page).
	$effect(() => {
		const ws = wsSlug;
		if (!ws) return;
		const uid = authStore.userId || null;
		const epochAtEntry = authStore.identityEpoch;
		untrack(() => void enterWorkspaceIndex(ws, uid, epochAtEntry));
	});

	// The layout ensures the collection list; this only notices that it failed,
	// so the page offers a retry instead of a skeleton that never resolves.
	// `ensureCollections` joins the layout's request rather than issuing one.
	async function ensurePageCollections(ws: string) {
		// The identity that asked (TASK-2203), and the workspace: a settle after
		// either moved describes a list this page is no longer showing.
		const isSameIdentity = authStore.identityFence();
		try {
			await collectionStore.ensureCollections(ws);
			if (ws !== wsSlug || !isSameIdentity()) return;
			collectionsError = null;
		} catch (err) {
			if (ws !== wsSlug || !isSameIdentity()) return;
			collectionsError = err;
		}
	}
	$effect(() => {
		const ws = wsSlug;
		if (!ws || collectionsReady) return;
		untrack(() => void ensurePageCollections(ws));
	});

	function retry() {
		collectionsError = null;
		if (!wsSlug) return;
		if (starredStore.error) void starredStore.load(wsSlug);
		void ensurePageCollections(wsSlug);
		void enterWorkspaceIndex(wsSlug, authStore.userId || null, authStore.identityEpoch);
	}

	// Starred ids, most recently starred first, that the index holds a live row
	// for. An id the index does not hold is an item this caller cannot see or
	// that was deleted, which the server's list left out too. Unstarring from a
	// card drops the id from the store, so the card leaves this list at once.
	let items = $derived.by<Item[]>(() => {
		if (loading) return [];
		const rows = new Map(localIndex.getAll(wsSlug).map((row) => [row.id, row]));
		const out: Item[] = [];
		for (const id of starredStore.ordered) {
			const row = rows.get(id);
			if (!row) continue;
			if (!includeTerminal && !isOpen(row, collections)) continue;
			out.push(row);
		}
		return out;
	});

	onMount(() => {
		workspaceStore.setCurrent(wsSlug);
	});

	// NO IDENTITY LISTENER HERE, deliberately (BUG-3005, lead ruling). The
	// layout's listener reloads on a real identity change, and the starred
	// store clears itself on one; everything this page shows derives from
	// those stores, so there is nothing of its own to reset.

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
	<PageHeader title="Starred" icon="⭐" count={items.length}>
		{#snippet actions()}
			<label class="terminal-toggle">
				<input type="checkbox" bind:checked={includeTerminal} />
				Show completed
			</label>
		{/snippet}
	</PageHeader>

	{#if loading}
		<div class="loading-state">
			<div class="skeleton-list">
				{#each Array(3) as _, i (i)}
					<div class="skeleton-card"></div>
				{/each}
			</div>
		</div>
	{:else if loadError}
		{@const failure = loadFailure('your starred items', loadError)}
		<ContentError
			title={failure.title}
			detail={failure.detail}
			onRetry={failure.retryable ? retry : undefined}
		/>
	{:else if items.length === 0}
		<EmptyState
			icon="☆"
			title="No starred items"
			message="Star items to keep track of things that matter to you. Click the ☆ on any item in a list or detail view to star it."
		/>
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
		max-width: var(--content-max-width);
		margin: 0 auto;
		padding: var(--space-8) var(--space-6);
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
