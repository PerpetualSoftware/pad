// Live refresh for Home, the agenda and Needs (DOC-4851 section 5).
//
// The access stream opts into `home_changed` and calls hint() for each one,
// and on a reconnect, since a reconnect cannot vouch for what it missed. A
// hint is a REFETCH HINT only: it carries no content, so consumers register a
// callback that refetches its own read, and nothing is ever applied from the
// hint itself.
//
// Hints arrive in bursts (one write fans out to several), so they are
// coalesced: a trailing DEBOUNCE_MS debounce, then at most one refetch per
// MIN_INTERVAL_MS. A burst inside the window is served by one trailing
// refetch when the window ends.

import { authStore } from '$lib/stores/auth.svelte';

export const HOME_LIVE_DEBOUNCE_MS = 750;
export const HOME_LIVE_MIN_INTERVAL_MS = 3000;

type Refetch = () => unknown;

function createHomeLive() {
	const consumers = new Set<Refetch>();
	let debounce: ReturnType<typeof setTimeout> | null = null;
	let held: ReturnType<typeof setTimeout> | null = null;
	let lastFire = -Infinity;
	let identityBound = false;

	function clear() {
		if (debounce) clearTimeout(debounce);
		if (held) clearTimeout(held);
		debounce = null;
		held = null;
	}

	function fire() {
		held = null;
		lastFire = Date.now();
		for (const refetch of [...consumers]) {
			try {
				void refetch();
			} catch {
				// One consumer failing must not starve the others.
			}
		}
	}

	function settle() {
		debounce = null;
		if (held) return; // a trailing refetch is already scheduled
		const wait = lastFire + HOME_LIVE_MIN_INTERVAL_MS - Date.now();
		if (wait <= 0) fire();
		else held = setTimeout(fire, wait);
	}

	function bindIdentity() {
		if (identityBound) return;
		identityBound = true;
		// A hint scheduled for the previous person refetches nothing for the next.
		authStore.onIdentityChange(() => {
			clear();
			lastFire = -Infinity;
		});
	}

	return {
		/** Register a refetch; returns the unregister function. */
		register(refetch: Refetch): () => void {
			bindIdentity();
			consumers.add(refetch);
			return () => {
				consumers.delete(refetch);
			};
		},
		/** A home_changed hint (or a reconnect) arrived. */
		hint() {
			bindIdentity();
			if (debounce) clearTimeout(debounce);
			debounce = setTimeout(settle, HOME_LIVE_DEBOUNCE_MS);
		},
		/** Test seam: drop timers and the rate window. */
		reset() {
			clear();
			lastFire = -Infinity;
			consumers.clear();
		}
	};
}

export const homeLive = createHomeLive();
