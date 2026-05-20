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

interface PendingRequest {
	parentRef: string;
	details: OpenChildrenDetails;
	resolve: (confirmed: boolean) => void;
}

let active = $state<PendingRequest | null>(null);
const queue: PendingRequest[] = [];

function advanceQueue(): void {
	active = queue.shift() ?? null;
}

/**
 * Surface the confirm dialog. Resolves true when the user clicks
 * "Override anyway", false when they cancel (button click, backdrop
 * click, or Escape).
 *
 * Multiple concurrent requests serialize — the second waits until
 * the first resolves before showing.
 */
function request(parentRef: string, details: OpenChildrenDetails): Promise<boolean> {
	return new Promise<boolean>((resolve) => {
		const entry: PendingRequest = { parentRef, details, resolve };
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

export const openChildrenDialog = {
	get active(): PendingRequest | null {
		return active;
	},
	request,
	confirm,
	cancel
};
