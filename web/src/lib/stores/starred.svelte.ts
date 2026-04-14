import { api } from '$lib/api/client';

// Set of starred item IDs for the current user in the current workspace
let starredIds = $state<Set<string>>(new Set());
let loaded = $state(false);
let currentWs = $state('');

// Monotonic request counter to discard stale responses on workspace switch
let requestSeq = 0;

export const starredStore = {
	get ids() { return starredIds; },
	get loaded() { return loaded; },

	isStarred(itemId: string): boolean {
		return starredIds.has(itemId);
	},

	/** Load all starred item IDs for a workspace. Call once on workspace load. */
	async load(wsSlug: string) {
		currentWs = wsSlug;
		const seq = ++requestSeq;

		try {
			const items = await api.items.starred(wsSlug, { include_terminal: true });
			// Discard if a newer load was initiated while this one was in flight
			if (seq !== requestSeq) return;
			starredIds = new Set(items.map(i => i.id));
			loaded = true;
		} catch {
			if (seq !== requestSeq) return;
			starredIds = new Set();
			loaded = true;
		}
	},

	/** Toggle star with optimistic update. */
	async toggle(wsSlug: string, itemSlug: string, itemId: string) {
		const wasStarred = starredIds.has(itemId);

		// Optimistic update
		const next = new Set(starredIds);
		if (wasStarred) {
			next.delete(itemId);
		} else {
			next.add(itemId);
		}
		starredIds = next;

		try {
			if (wasStarred) {
				await api.items.unstar(wsSlug, itemSlug);
			} else {
				await api.items.star(wsSlug, itemSlug);
			}
		} catch {
			// Revert on failure (only if still on the same workspace)
			if (currentWs !== wsSlug) return;
			const reverted = new Set(starredIds);
			if (wasStarred) {
				reverted.add(itemId);
			} else {
				reverted.delete(itemId);
			}
			starredIds = reverted;
		}
	},

	clear() {
		starredIds = new Set();
		loaded = false;
		currentWs = '';
	}
};
