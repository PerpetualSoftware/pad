import { api } from '$lib/api/client';
import type { Collection, Item } from '$lib/types';

let collections = $state<Collection[]>([]);
let items = $state<Item[]>([]);
// Workspace slug the current `items` array was loaded for. Used to decide
// whether `items` is stale for a different workspace (BUG-1461). `items` is
// a single global slot, so without this stamp a navigation from workspace A
// to workspace B would leave A's items in the store and freshness checks
// based purely on `items.length` would silently accept them as B's data —
// causing wiki-link resolution (which scans `items` for title/ref matches)
// to render `[[X]]` brackets as plain text. Null = no load completed yet
// for a full workspace; a non-null value pairs the items array with its
// source workspace.
let itemsWorkspace = $state<string | null>(null);
// Workspace slug the current `collections` array was loaded for — the
// collections analogue of `itemsWorkspace`. `collections` is likewise a
// single global slot that retains the previous workspace's data while
// `loadCollections()` for the new workspace is in flight. A consumer that
// pairs the collections with a specific `wsSlug` — e.g. TASK-2160's editor
// content-link gate maps collection slug → ref prefix and validates a link
// against the CURRENT workspace — must be able to tell "these collections
// are actually this workspace's" from "stale from the last workspace"
// (Codex review). Null = no load completed yet.
let collectionsWorkspace = $state<string | null>(null);
let activeItem = $state<Item | null>(null);
let loading = $state(false);

export const collectionStore = {
	get collections() { return collections; },
	get items() { return items; },
	get itemsWorkspace() { return itemsWorkspace; },
	get collectionsWorkspace() { return collectionsWorkspace; },
	get activeItem() { return activeItem; },
	get loading() { return loading; },

	/**
	 * Returns true when `items` was last loaded as a full-workspace list
	 * (no collection filter) for the given workspace slug. Callers that
	 * need ALL items for wiki-link resolution should use this gate rather
	 * than `items.length === 0` — the latter can't distinguish "empty
	 * workspace" from "stale items from a different workspace."
	 */
	itemsAreFreshFor(ws: string): boolean {
		return itemsWorkspace === ws;
	},

	/**
	 * Returns true when the `collections` array was last loaded for the given
	 * workspace slug — the collections analogue of `itemsAreFreshFor`. A
	 * consumer that pairs collection metadata (e.g. slug → prefix) with a
	 * specific workspace must gate on this rather than trust a possibly-stale
	 * global array mid workspace-switch (Codex review, TASK-2160).
	 */
	collectionsAreFreshFor(ws: string): boolean {
		return collectionsWorkspace === ws;
	},

	get defaultCollections() {
		return collections.filter(c => c.is_default).sort((a, b) => a.sort_order - b.sort_order);
	},

	get customCollections() {
		return collections.filter(c => !c.is_default).sort((a, b) => a.sort_order - b.sort_order);
	},

	async loadCollections(ws: string) {
		loading = true;
		try {
			collections = await api.collections.list(ws);
			// Stamp the array with its source workspace so consumers can tell
			// it apart from a stale previous-workspace load (see
			// `collectionsAreFreshFor`). Set only on success — a failed load
			// leaves the prior (possibly stale) array in place, and its stamp
			// with it, which is the correct conservative signal.
			collectionsWorkspace = ws;
		} finally {
			loading = false;
		}
	},

	async loadItems(ws: string, collectionSlug?: string, params?: Record<string, string | number | boolean | undefined>) {
		loading = true;
		try {
			if (collectionSlug) {
				items = await api.items.listByCollection(ws, collectionSlug, params);
				// Partial load — the stored array no longer represents the
				// full workspace, so invalidate the freshness stamp. A
				// downstream caller asking `itemsAreFreshFor(ws)` will
				// correctly trigger a full re-load.
				itemsWorkspace = null;
			} else {
				items = await api.items.list(ws, params);
				itemsWorkspace = ws;
			}
		} finally {
			loading = false;
		}
	},

	async loadItem(ws: string, slug: string) {
		activeItem = await api.items.get(ws, slug);
		return activeItem;
	},

	setActiveItem(item: Item | null) {
		activeItem = item;
	},

	addItem(item: Item) {
		if (!items.find(i => i.id === item.id)) {
			items = [...items, item];
		}
	},

	updateItemInList(item: Item) {
		items = items.map(i => i.id === item.id ? item : i);
		if (activeItem?.id === item.id) {
			activeItem = item;
		}
	},

	removeItem(slug: string) {
		items = items.filter(i => i.slug !== slug);
		if (activeItem?.slug === slug) {
			activeItem = null;
		}
	},
};
