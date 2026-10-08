import { toastStore } from '$lib/stores/toast.svelte';

/**
 * "Can't reach the server" toast for requests whose fetch rejected before any
 * response (TASK-2202). The API client fires `setNetworkUnreachableHandler`
 * once per such request; +layout.svelte wires that seam here, the same split
 * as serverBusyToast, so client.ts stays free of the toast store.
 *
 * It exists because most call sites show a fixed string ("Failed to save")
 * whatever went wrong, so an outage and a validation refusal looked the same.
 * This says WHY, once, beside whatever the call site shows.
 *
 * Deduped: an outage fails every in-flight request at once, and a stack of
 * identical toasts helps nobody. At most one inside DEDUPE_WINDOW_MS.
 */

const DEDUPE_WINDOW_MS = 15_000;

export const NETWORK_UNREACHABLE_MESSAGE =
	"Can't reach the server. Check your connection; changes made now may not be saved.";

let lastShownAt = Number.NEGATIVE_INFINITY;

/**
 * Show the deduped error toast. An error, so it is announced assertively and
 * lingers (toast store defaults). Returns whether a toast was shown; `now` is
 * injectable for tests.
 */
export function notifyNetworkUnreachable(now: number = Date.now()): boolean {
	if (now - lastShownAt < DEDUPE_WINDOW_MS) return false;
	lastShownAt = now;
	toastStore.show(NETWORK_UNREACHABLE_MESSAGE, 'error');
	return true;
}

/** Test-only: reset the dedupe clock. */
export function __resetNetworkUnreachableToastForTest(): void {
	lastShownAt = Number.NEGATIVE_INFINITY;
}
