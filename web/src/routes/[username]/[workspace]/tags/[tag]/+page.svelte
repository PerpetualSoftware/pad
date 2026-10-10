<script lang="ts">
	import { page } from '$app/state';
	import { titleStore } from '$lib/stores/title.svelte';
	import { untrack } from 'svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { localIndex } from '$lib/stores/localIndex.svelte';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { enterWorkspaceIndex } from '$lib/stores/workspaceIndexEntry';
	import { hasTag, isOpen, byPinnedThenRecent } from '$lib/items/localLists';
	import { createScrollRestoration } from '$lib/scroll/restore.svelte';
	import { browser } from '$app/env';
	import ItemCard from '$lib/components/collections/ItemCard.svelte';
	import EmptyState from '$lib/components/common/EmptyState.svelte';
	import ContentError from '$lib/components/common/ContentError.svelte';
	import { loadFailure } from '$lib/api/loadFailure';
	import type { Item, Collection } from '$lib/types';

	type ViewMode = 'list' | 'board';

	let wsSlug = $derived(page.params.workspace ?? '');
	// Route params arrive URL-decoded, so this is the human-readable tag.
	let tag = $derived(page.params.tag ?? '');

	// Served from the workspace's local index (TASK-2231). The page used to
	// fetch every tagged item with its full body (~500 KB for a 50-item tag)
	// and its own copy of the collection list, to render card summaries the
	// index already holds; now it makes no request of its own. The index is
	// kept current by the workspace's live sync, so the list also follows
	// edits made elsewhere, which the one-shot fetch never did.
	let indexState = $derived(wsSlug ? localIndex.bootstrapStateFor(wsSlug) : 'cold');
	// The collection list the workspace layout keeps, gated on being THIS
	// workspace's: mid-switch the array still holds the previous one's.
	let collectionsReady = $derived(!!wsSlug && collectionStore.collectionsAreFreshFor(wsSlug));
	let collections = $derived<Collection[]>(collectionsReady ? collectionStore.collections : []);
	// A revoked caller's index is RESET, not failed: its state goes back to
	// 'cold' and only the store's memo says why (the collection page reads the
	// same signal). Without it this page would show the skeleton forever.
	let accessRevoked = $derived(!!wsSlug && localIndex.accessRevokedFor(wsSlug));
	// TASK-2203: a failed load is an error with a retry, never "No items tagged".
	let collectionsError = $state<unknown>(null);
	let loadError = $derived<unknown>(
		accessRevoked ? { code: 'forbidden' } : indexState === 'error' ? new Error() : collectionsError
	);
	// The index AND the collection list, because the completed filter judges
	// each item by its collection's done field: before the list lands it would
	// judge by the defaults.
	let loading = $derived(!loadError && (indexState !== 'ready' || !collectionsReady));

	// View mode persists per workspace (list = grouped sections, board = one
	// lane per collection). localStorage-backed; guarded for SSR.
	const viewStorageKey = $derived(`pad-tag-view-${wsSlug}`);
	let viewMode = $state<ViewMode>('list');
	$effect(() => {
		if (!browser) return;
		// localStorage can throw when storage is disabled/blocked (private
		// mode, embedded contexts); fall back to the default view.
		try {
			const saved = localStorage.getItem(viewStorageKey);
			viewMode = saved === 'board' ? 'board' : 'list';
		} catch {
			viewMode = 'list';
		}
	});
	function setViewMode(mode: ViewMode) {
		viewMode = mode; // in-session preference applies regardless of persistence
		if (!browser) return;
		try {
			localStorage.setItem(viewStorageKey, mode);
		} catch {
			// Storage unavailable/full — keep the in-session choice only.
		}
	}

	// Completed items (TASK-2211, audit C105). A tag page used to mix them in
	// with no way to leave them out, so a long-lived tag read as open work.
	// "Show completed" defaults ON, as the collection list shows completed
	// items, and is remembered per workspace like the view mode above. Off,
	// the list asks the server for non-terminal items only (non_terminal=true,
	// resolved per collection from each schema's terminal options).
	const completedStorageKey = $derived(`pad-tag-completed-${wsSlug}`);
	let showCompleted = $state(true);
	$effect(() => {
		if (!browser) return;
		try {
			showCompleted = localStorage.getItem(completedStorageKey) !== 'hide';
		} catch {
			showCompleted = true;
		}
	});
	function setShowCompleted(show: boolean) {
		showCompleted = show;
		if (!browser) return;
		try {
			localStorage.setItem(completedStorageKey, show ? 'show' : 'hide');
		} catch {
			// Storage unavailable — keep the in-session choice only.
		}
	}

	const scrollRestoration = createScrollRestoration({
		ready: () => !loading,
		persistKey: () => (wsSlug ? `pad-last-scroll-${wsSlug}-${page.url.pathname}` : null)
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
		void ensurePageCollections(wsSlug);
		void enterWorkspaceIndex(wsSlug, authStore.userId || null, authStore.identityEpoch);
	}

	let fetchedItems = $derived.by<Item[]>(() => {
		if (loading || !tag) return [];
		const tagged = localIndex.getAll(wsSlug).filter((item) => hasTag(item, tag));
		const shown = showCompleted ? tagged : tagged.filter((item) => isOpen(item, collections));
		return shown.sort(byPinnedThenRecent);
	});

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

	// The browser tab names this section (TASK-2261, audit C99). Pathname is
	// read so a reuse across workspaces re-sets it (see activity/+page.svelte).
	$effect(() => {
		page.url.pathname;
		titleStore.setPageTitle({ section: tag ? `#${tag}` : 'Tags', item: null });
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
		<div class="page-header-right">
			<label class="completed-toggle">
				<input
					type="checkbox"
					checked={showCompleted}
					onchange={(e) => setShowCompleted(e.currentTarget.checked)}
				/>
				Show completed
			</label>
			{#if fetchedItems.length > 0}
				<div class="view-toggle" role="group" aria-label="View mode">
					<button
						type="button"
						class="view-btn"
						class:active={viewMode === 'list'}
						onclick={() => setViewMode('list')}
					>
						List
					</button>
					<button
						type="button"
						class="view-btn"
						class:active={viewMode === 'board'}
						onclick={() => setViewMode('board')}
					>
						Board
					</button>
				</div>
			{/if}
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
	{:else if loadError}
		{@const failure = loadFailure('the items with this tag', loadError)}
		<ContentError
			title={failure.title}
			detail={failure.detail}
			onRetry={failure.retryable ? retry : undefined}
		/>
	{:else if fetchedItems.length === 0}
		{#if showCompleted}
			<EmptyState
				icon="🏷"
				title={`No items tagged “${tag}”`}
				message="Add this tag to an item from its detail page to group it here."
			/>
		{:else}
			<EmptyState
				icon="🏷"
				title={`No open items tagged “${tag}”`}
				message="Completed items are hidden. Turn on “Show completed” to see them."
			/>
		{/if}
	{:else if viewMode === 'board'}
		<!-- Board: one lane per collection (the shared axis). Read-only — no
		     drag/status changes, since moving a card between collection lanes
		     would mean reclassifying the item, not the intent here. -->
		<div class="tag-board">
			{#each groupedItems as group (group.collection.id)}
				<div class="board-lane">
					<div class="lane-header">
						<span class="group-icon">{group.collection.icon || '📁'}</span>
						<span class="group-name">{group.collection.name}</span>
						<span class="group-count">{group.items.length}</span>
					</div>
					<div class="lane-items">
						{#each group.items as item (item.id)}
							<ItemCard {item} collection={group.collection} compact={true} showCollection={false} />
						{/each}
					</div>
				</div>
			{/each}
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

	.page-header-right {
		display: flex;
		align-items: center;
		gap: var(--space-3);
	}
	.completed-toggle {
		display: inline-flex;
		align-items: center;
		gap: var(--space-1);
		font-size: 0.85rem;
		color: var(--text-secondary);
		white-space: nowrap;
		cursor: pointer;
	}
	.view-toggle {
		display: inline-flex;
		border: 1px solid var(--border);
		border-radius: var(--radius-sm, 6px);
		overflow: hidden;
	}

	.view-btn {
		padding: 0.3em 0.85em;
		font-size: 0.8em;
		background: var(--bg-primary);
		color: var(--text-secondary);
		border: none;
		cursor: pointer;
	}

	.view-btn + .view-btn {
		border-left: 1px solid var(--border);
	}

	/* Selected segment: a 1px accent ring (BUG-3420). Background alone measured
	   1.05-1.16:1 against its neighbours in both themes; the ring is >= 4.4:1. */
	.view-btn.active {
		background: var(--bg-tertiary);
		color: var(--text-primary);
		font-weight: 600;
		box-shadow: inset 0 0 0 1px var(--accent-primary);
	}

	.tag-board {
		display: flex;
		gap: var(--space-4);
		overflow-x: auto;
		align-items: flex-start;
		padding-bottom: var(--space-2);
	}

	.board-lane {
		display: flex;
		flex-direction: column;
		flex: 1 0 0;
		min-width: 240px;
		max-width: 320px;
	}

	.lane-header {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-2);
		position: sticky;
		top: 0;
	}

	.lane-items {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
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
