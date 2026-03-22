import { api } from '$lib/api/client';
import type { Collection, Item } from '$lib/types';

let collections = $state<Collection[]>([]);
let items = $state<Item[]>([]);
let activeItem = $state<Item | null>(null);
let loading = $state(false);

export const collectionStore = {
	get collections() { return collections; },
	get items() { return items; },
	get activeItem() { return activeItem; },
	get loading() { return loading; },

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
		} finally {
			loading = false;
		}
	},

	async loadItems(ws: string, collectionSlug?: string, params?: Record<string, string | number | boolean | undefined>) {
		loading = true;
		try {
			if (collectionSlug) {
				items = await api.items.listByCollection(ws, collectionSlug, params);
			} else {
				items = await api.items.list(ws, params);
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
