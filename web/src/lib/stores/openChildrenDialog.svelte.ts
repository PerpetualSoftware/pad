// BUG-1538 / TASK-1539 — Singleton state for the open-children-guard
// confirm dialog. Mounted once in +layout.svelte via
// <OpenChildrenDialog />; producer code (the API error helper) calls
// `openChildrenDialog.request(...)` to surface a modal and awaits the
// user's choice. Decoupling the producer from the renderer keeps call
// sites in collection/detail pages free of dialog markup.
//
// The store enforces a single in-flight prompt — concurrent calls
// queue. In practice an item PATCH only fires one at a time per user
// action, so this matters mostly for the BoardView drag-drop case
// where a quick second drop could fire before the first confirm
// resolves; queueing avoids dropping the second prompt on the floor.

import type { OpenChildrenDetails } from '$lib/items/openChildrenError';
import { authStore } from './auth.svelte';

interface PendingRequest {
	parentRef: string;
	details: OpenChildrenDetails;
	resolve: (confirmed: boolean) => void;
	/**
	 * Whether the question is still worth asking, supplied by the REQUESTER
	 * (BUG-3046): only it knows its pane identity and write order. Asked when
	 * a queued entry is about to be SHOWN; absent means always live.
	 */
	isLive?: () => boolean;
}

let active = $state<PendingRequest | null>(null);
const queue: PendingRequest[] = [];

/**
 * Show the next QUEUED request that is still live (BUG-3046). A request queued
 * behind another is not withdrawn when its reason disappears: the user
 * switched items, or a newer edit superseded the one that raised it. Showing it
 * asked the user to decide something no longer live. A dead entry resolves
 * FALSE, the cancel answer, so its producer takes the cancel path, which is
 * what the producer's own fenced retry would have ended in anyway.
 */
function advanceQueue(): void {
	for (let next = queue.shift(); next; next = queue.shift()) {
		if (next.isLive && !next.isLive()) {
			next.resolve(false);
			continue;
		}
		active = next;
		return;
	}
	active = null;
}

/**
 * Surface the confirm dialog. Resolves true when the user clicks
 * "Override anyway", false when they cancel (button click, backdrop
 * click, or Escape).
 *
 * Multiple concurrent requests serialize — the second waits until
 * the first resolves before showing.
 */
function request(
	parentRef: string,
	details: OpenChildrenDetails,
	isLive?: () => boolean
): Promise<boolean> {
	return new Promise<boolean>((resolve) => {
		const entry: PendingRequest = { parentRef, details, resolve, isLive };
		if (active === null) {
			active = entry;
		} else {
			queue.push(entry);
		}
	});
}

function confirm(): void {
	const a = active;
	if (!a) return;
	a.resolve(true);
	advanceQueue();
}

function cancel(): void {
	const a = active;
	if (!a) return;
	a.resolve(false);
	advanceQueue();
}

/**
 * Abandon the active prompt and everything queued behind it (BUG-3005).
 *
 * Every pending request resolves FALSE — the same answer a cancel gives — so
 * the producer awaiting it takes its cancel path and does not perform the
 * mutation. Rejecting instead would surface an unhandled rejection at call
 * sites that only ever expected a boolean.
 */
function abandonAll(): void {
	const pending = active ? [active, ...queue] : [...queue];
	queue.length = 0;
	active = null;
	for (const entry of pending) entry.resolve(false);
}

export const openChildrenDialog = {
	get active(): PendingRequest | null {
		return active;
	},
	request,
	confirm,
	cancel,
	abandonAll
};

// This dialog is mounted in the ROOT layout, outside the workspace subtree the
// identity remount covers, and it holds two things that must not cross an
// identity change (BUG-3005, codex round 3):
//
//   - `details`, the FETCHED child list from the 409 — B would read A's child
//     titles and statuses out of an open dialog;
//   - `resolve`, a continuation that PERFORMS A'S MUTATION when confirmed. That
//     is a write under the wrong identity, not a display leak, and it is the
//     one place on this branch where the item's own bound ("nothing here is an
//     authorization bypass") would stop being true.
//
// Resolving false is what makes the second half safe: the producer sees a
// cancel and abandons the write, rather than being left with a promise that
// never settles.
authStore.onIdentityChange(() => {
	openChildrenDialog.abandonAll();
});
