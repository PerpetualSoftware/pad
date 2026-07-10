import { toastStore } from '$lib/stores/toast.svelte';

/**
 * "Server busy" toast handler for surfaced 429 `rate_limited` errors
 * (TASK-2080 / PLAN-1984). The API client (`client.ts`) fires
 * `setRateLimitHandler` once per surfaced 429; +layout.svelte wires that
 * seam to `notifyServerBusy` here. Keeping this OUT of client.ts is
 * deliberate — client.ts stays free of any Svelte/toast-store import
 * (mirrors the `setAccessRevokedHandler` split).
 *
 * DEDUPE is this handler's whole reason for existing: a busy server 429s a
 * BURST of independent in-flight requests at once, and the client faithfully
 * fires the seam for each one. Without a throttle the user would get a stack
 * of ~10 identical "server busy" toasts. We show at most one within
 * `DEDUPE_WINDOW_MS`.
 */

// Suppress repeat busy-toasts inside this window. Slightly longer than the
// toast's own on-screen lifetime so a sustained 429 storm can't refresh a
// second toast the instant the first auto-dismisses.
const DEDUPE_WINDOW_MS = 6_000;

// The busy toast lingers a touch longer than the 3s default so the user
// actually reads it before it clears.
const BUSY_TOAST_DURATION_MS = 5_000;

// Epoch ms of the last shown busy toast. Module-level so dedupe spans every
// call site funneling through the one registered handler. Seeded to
// -Infinity (not 0) so the very first notification always fires — otherwise a
// caller passing now=0 (tests) would collide with a 0 initial value.
let lastShownAt = Number.NEGATIVE_INFINITY;

/**
 * Human-readable, non-alarming busy message. When the server handed us a
 * usable `Retry-After` (>= ~1s) we name the wait; otherwise we stay vague
 * ("in a moment") rather than invent a number.
 */
export function serverBusyMessage(retryAfterMs?: number): string {
	const secs = retryAfterMs != null && retryAfterMs > 0 ? Math.round(retryAfterMs / 1000) : 0;
	if (secs >= 1) {
		return `Server busy — please try again in ${secs}s.`;
	}
	return 'Server busy — please try again in a moment.';
}

/**
 * Show the deduped "server busy" toast. Returns true when a toast was
 * actually shown, false when it was suppressed as a duplicate inside the
 * dedupe window. `now` is injectable for deterministic tests; production
 * callers omit it. TASK-2080.
 */
export function notifyServerBusy(retryAfterMs?: number, now: number = Date.now()): boolean {
	if (now - lastShownAt < DEDUPE_WINDOW_MS) return false;
	lastShownAt = now;
	// 'info' is the least-alarming severity the toast store offers — a busy
	// server is transient, not an error the user did anything wrong to cause.
	toastStore.show(serverBusyMessage(retryAfterMs), 'info', BUSY_TOAST_DURATION_MS);
	return true;
}

/**
 * Test-only reset of the dedupe clock so each test starts from a clean
 * "no toast shown yet" state. Not used in production.
 */
export function __resetServerBusyToastForTest(): void {
	lastShownAt = Number.NEGATIVE_INFINITY;
}
