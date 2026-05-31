<script lang="ts">
	// Public read-only collection view (TASK-1679 / PLAN-1677).
	//
	// The single entry point for rendering a shared collection on the `/s/[token]`
	// page (wired by TASK-1680). Takes the RAW share-link payload — `collection`
	// and `items` exactly as the backend (TASK-1678) sends them — parses
	// defensively (settings/schema/fields may be JSON strings OR objects), and
	// delegates to the board / list / table renderer matching the owner's
	// `settings.default_view`.
	//
	// Either pass raw payload pieces (`collection` + `items`) for the switcher to
	// normalize, OR pre-parsed `parsedCollection` + `parsedItems` if the caller
	// already normalized. The raw path is the common case.
	//
	// Read-only: no chrome, no edit/drag/create, no internal links. An optional
	// `view` prop overrides the default view (the read-only view switcher,
	// Phase 2 / TASK-1680, will drive this); `onactivate` + `expandable` are
	// forwarded to the leaf renderers for the deferred inline expand (TASK-1684).
	import type { PublicCollection, PublicItem } from './shareView';
	import { parsePublicCollection, parsePublicItems } from './shareView';
	import PublicBoardView from './PublicBoardView.svelte';
	import PublicListView from './PublicListView.svelte';
	import PublicTableView from './PublicTableView.svelte';

	interface Props {
		/** Raw `collection` branch of the share payload (string/object tolerant). */
		collection?: unknown;
		/** Raw `items` array of the share payload. */
		items?: unknown;
		/** Pre-parsed collection — supply instead of `collection` to skip parsing. */
		parsedCollection?: PublicCollection;
		/** Pre-parsed items — supply instead of `items` to skip parsing. */
		parsedItems?: PublicItem[];
		/** Override the rendered view; defaults to settings.default_view. */
		view?: 'list' | 'board' | 'table';
		/** Forwarded to leaf renderers for the deferred inline expand (TASK-1684). */
		expandable?: boolean;
		onactivate?: (item: PublicItem) => void;
	}

	let {
		collection,
		items,
		parsedCollection,
		parsedItems,
		view,
		expandable = false,
		onactivate
	}: Props = $props();

	let coll = $derived<PublicCollection>(parsedCollection ?? parsePublicCollection(collection));
	let list = $derived<PublicItem[]>(parsedItems ?? parsePublicItems(items));
	let activeView = $derived(view ?? coll.settings.default_view);
</script>

<div class="public-collection">
	<header class="collection-header">
		{#if coll.icon}<span class="collection-icon">{coll.icon}</span>{/if}
		<h1 class="collection-name">{coll.name}</h1>
	</header>
	{#if coll.description}
		<p class="collection-description">{coll.description}</p>
	{/if}

	{#if list.length === 0}
		<p class="collection-empty">No items in this collection.</p>
	{:else if activeView === 'board'}
		<PublicBoardView collection={coll} items={list} {expandable} {onactivate} />
	{:else if activeView === 'table'}
		<PublicTableView collection={coll} items={list} {expandable} {onactivate} />
	{:else}
		<PublicListView collection={coll} items={list} {expandable} {onactivate} />
	{/if}
</div>

<style>
	.public-collection {
		display: flex;
		flex-direction: column;
		gap: var(--space-5);
		min-width: 0;
	}

	.collection-header {
		display: flex;
		align-items: center;
		gap: var(--space-3);
	}

	.collection-icon {
		font-size: 1.6em;
		line-height: 1;
	}

	.collection-name {
		font-size: 1.8em;
		font-weight: 700;
		letter-spacing: -0.02em;
		margin: 0;
	}

	.collection-description {
		color: var(--text-secondary);
		font-size: 0.95em;
		margin: 0;
	}

	.collection-empty {
		color: var(--text-muted);
		font-size: 0.9em;
		padding: var(--space-4) 0;
		margin: 0;
	}
</style>
