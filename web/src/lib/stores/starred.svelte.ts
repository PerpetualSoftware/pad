import { api } from '$lib/api/client';
import { authStore } from './auth.svelte';

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
		// TWO FENCES, for two races (BUG-3005). `seq` is the navigation fence: a
		// slower load for workspace A must not overwrite a faster one for B.
		// The identity fence is a different question — this list is PER USER, so
		// a response issued as A must not land in B's session even when the
		// workspace never changed. Neither subsumes the other: a same-route
		// account swap moves no workspace, and a workspace switch moves no
		// identity.
		const isSameIdentity = authStore.identityFence();
		pendingToggles.clear();
		// Clear stale state immediately to avoid showing a previous user/workspace's stars
		starredIds = new Set();
		loaded = false;

		try {
			const items = await api.items.starred(wsSlug, { include_terminal: true });
			if (seq !== requestSeq || !isSameIdentity()) return;
			starredIds = applyPendingToggles(new Set(items.map(i => i.id)));
			loaded = true;
		} catch {
			if (seq !== requestSeq || !isSameIdentity()) return;
			// Preserve any optimistic toggles even if the load failed
			starredIds = applyPendingToggles(new Set());
			loaded = true;
		}
	},

	/** Toggle star with optimistic update. Serialized per item. */
	async toggle(wsSlug: string, itemSlug: string, itemId: string) {
		// Drop rapid duplicate clicks while a toggle is in flight for this item
		if (toggleInFlight.has(itemId)) return;

		const isSameIdentity = authStore.identityFence();
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
			// Revert on failure — only if this is still the same workspace AND
			// the same signed-in user. Without the identity half, a star request
			// that fails after an account swap writes A's item id into B's
			// starred set (BUG-3005).
			if (currentWs !== wsSlug || !isSameIdentity()) return;
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

// Drop this user's stars the moment the signed-in user changes (BUG-3005).
//
// `load()` clears before it fetches, which covers a workspace SWITCH — but it
// is keyed by workspace slug and a same-route account swap starts no new load,
// so A's starred ids stayed on screen for B until something else happened to
// trigger one. The fences above cover a settle that RACES the change; this
// covers the case where nothing is racing at all, which is the common one.
//
// Module scope, registered once, never unsubscribed: this store is a singleton
// and its lifetime is the tab's.
authStore.onIdentityChange(() => {
	starredStore.clear();
});
