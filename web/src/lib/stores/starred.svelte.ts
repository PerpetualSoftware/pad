import { api } from '$lib/api/client';

// Set of starred item IDs for the current user in the current workspace
let starredIds = $state<Set<string>>(new Set());
let loaded = $state(false);
let currentWs = $state('');

// Monotonic request counter to discard stale responses on workspace switch
let requestSeq = 0;

// Track toggles that happened while a load is in flight, so the load
// result can be merged with local mutations instead of overwriting them.
// Map of itemId → true (starred) or false (unstarred) since load started.
let pendingToggles = new Map<string, boolean>();

// Per-item toggle locks to serialize rapid clicks on the same item
let toggleInFlight = new Set<string>();

/** Apply pending toggles on top of a base set. */
function applyPendingToggles(base: Set<string>): Set<string> {
	for (const [itemId, starred] of pendingToggles) {
		if (starred) {
			base.add(itemId);
		} else {
			base.delete(itemId);
		}
	}
	pendingToggles.clear();
	return base;
}

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
		pendingToggles.clear();

		try {
			const items = await api.items.starred(wsSlug, { include_terminal: true });
			if (seq !== requestSeq) return;
			starredIds = applyPendingToggles(new Set(items.map(i => i.id)));
			loaded = true;
		} catch {
			if (seq !== requestSeq) return;
			// Preserve any optimistic toggles even if the load failed
			starredIds = applyPendingToggles(new Set());
			loaded = true;
		}
	},

	/** Toggle star with optimistic update. Serialized per item. */
	async toggle(wsSlug: string, itemSlug: string, itemId: string) {
		// Drop rapid duplicate clicks while a toggle is in flight for this item
		if (toggleInFlight.has(itemId)) return;

		const wasStarred = starredIds.has(itemId);
		const nowStarred = !wasStarred;

		// Track this toggle so an in-flight load can merge it
		pendingToggles.set(itemId, nowStarred);

		// Optimistic update
		const next = new Set(starredIds);
		if (nowStarred) {
			next.add(itemId);
		} else {
			next.delete(itemId);
		}
		starredIds = next;

		toggleInFlight.add(itemId);
		try {
			if (wasStarred) {
				await api.items.unstar(wsSlug, itemSlug);
			} else {
				await api.items.star(wsSlug, itemSlug);
			}
		} catch {
			// Revert on failure (only if still on the same workspace)
			if (currentWs !== wsSlug) return;
			pendingToggles.set(itemId, wasStarred);
			const reverted = new Set(starredIds);
			if (wasStarred) {
				reverted.add(itemId);
			} else {
				reverted.delete(itemId);
			}
			starredIds = reverted;
		} finally {
			toggleInFlight.delete(itemId);
		}
	},

	clear() {
		starredIds = new Set();
		loaded = false;
		currentWs = '';
		pendingToggles.clear();
		toggleInFlight.clear();
	}
};
