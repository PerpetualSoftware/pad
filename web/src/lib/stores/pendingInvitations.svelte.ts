// Pending invitations addressed to the signed-in account (BUG-2136 U2), kept
// for the TopBar "+" badge so an invitation is visible without opening
// anything. Self-hosted instances may send no email, so this badge IS the
// notification (lead ruling, day 87: no new SSE kind; refetch on load, window
// focus and route change, and the invitee sees it on their next page view).
//
// The list itself is rendered by PendingInvitations inside the "+" surface,
// which fetches on open and reports what it got back here with set(), and
// remove() after an accept or decline, so the badge never lags the list.
import { api } from '$lib/api/client';
import { authStore } from '$lib/stores/auth.svelte';
import type { MyInvitation } from '$lib/types';

// The shortest gap between two refetches a focus or a route change may cause.
// Navigation is frequent and the general API bucket is shared with the page's
// own loads (BUG-3192), so a burst of route changes costs at most one request
// per window. Load and an explicit refresh(true) bypass it.
export const MIN_REFETCH_INTERVAL_MS = 15_000;

class PendingInvitationsStore {
	invitations = $state<MyInvitation[]>([]);
	// Orders responses: an older fetch cannot re-surface an invitation a newer
	// fetch, set() or remove() has already dropped.
	#seq = 0;
	#lastFetch = 0;

	constructor() {
		// A different account's invitations must never be shown. A swap reloads
		// the tab anyway; this covers sign-out, which does not.
		authStore.onIdentityChange(() => {
			this.#seq++;
			this.#lastFetch = 0;
			this.invitations = [];
		});
	}

	get count(): number {
		return this.invitations.length;
	}

	/** Refetch, unless one ran within MIN_REFETCH_INTERVAL_MS (force skips that). */
	async refresh(force = false, now = Date.now()): Promise<void> {
		if (!authStore.userId) return;
		if (!force && now - this.#lastFetch < MIN_REFETCH_INTERVAL_MS) return;
		this.#lastFetch = now;
		const mine = ++this.#seq;
		const isSameIdentity = authStore.identityFence();
		try {
			const res = await api.members.listMyInvitations();
			if (mine === this.#seq && isSameIdentity()) this.invitations = res.invitations ?? [];
		} catch {
			// Keep the last list: a failed refetch is not "no invitations".
		}
	}

	/** The list PendingInvitations just fetched. */
	set(list: MyInvitation[]): void {
		this.#seq++;
		this.invitations = list;
	}

	/** An invitation was accepted or declined here. */
	remove(id: string): void {
		this.#seq++;
		this.invitations = this.invitations.filter((i) => i.id !== id);
	}
}

export const pendingInvitations = new PendingInvitationsStore();
