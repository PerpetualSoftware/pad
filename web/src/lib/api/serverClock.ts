/**
 * An estimate of the SERVER's clock, for stamping the sync cursor without ever
 * reading the client's wall clock (BUG-3207).
 *
 * The sync cursor is compared against server-side `updated_at` values, so a
 * cursor taken from the client clock skips every change committed in the
 * skew window whenever the client runs ahead. The API client feeds this module
 * the `Date` header of each response it receives; `estimateServerNow` projects
 * the newest one forward by the MONOTONIC time elapsed since it arrived.
 *
 * THE ESTIMATE ONLY ERRS EARLY, by construction. The trade is deliberate: an
 * early cursor re-delivers changes the reader already has (a duplicate, which
 * every sync consumer tolerates), while a late one skips changes nobody will
 * ever deliver (a miss, which nothing recovers). Each term keeps the bias:
 *
 *   - `Date` is truncated to the second, so it is at or before the instant the
 *     server wrote the response;
 *   - that instant is at or before the instant the client received it, so
 *     `Date + (now − receivedAt)` is at or before the server's clock now;
 *   - `performance.now()` is monotonic. Where it undercounts (some platforms
 *     stop it while the OS sleeps) the estimate falls further behind, which is
 *     the SAFE direction. Do not "fix" that with `Date.now()`: the client wall
 *     clock is the thing this module exists to keep out of the cursor;
 *   - a further `RESOLUTION_MARGIN_MS` is subtracted for the header's
 *     one-second resolution, as ruled on the trail.
 *
 * `null` until a response carrying a parseable `Date` has been seen; callers
 * then ask the server directly (see `syncService.stamp`).
 */

/** Subtracted from every estimate: the `Date` header's resolution. */
export const RESOLUTION_MARGIN_MS = 1000;

let sample: { serverMs: number; receivedAt: number } | null = null;

/** Record a response's `Date` header. Keeps the sample with the newest server time. */
export function noteServerDate(dateHeader: string | null | undefined, receivedAt = performance.now()): void {
	if (!dateHeader) return;
	const serverMs = Date.parse(dateHeader);
	if (!Number.isFinite(serverMs)) return;
	// Compare PROJECTED times, not raw ones: an older sample received long ago
	// can project past a newer one, and the newer one is the tighter bound.
	if (sample && projected(sample, receivedAt) >= serverMs) return;
	sample = { serverMs, receivedAt };
}

/**
 * The server's clock now, erring early; `null` with no sample yet.
 *
 * A WHOLE number of milliseconds: `performance.now()` is fractional, and
 * handleGetChanges parses `since` with ParseInt, so a fractional cursor made
 * every /changes asked from it a 400 that degraded to a full reload (BUG-3207
 * checkpoint 8). Floor, never round: rounding can move the estimate up, and
 * this module's one property is that it never runs ahead.
 */
export function estimateServerNow(now = performance.now()): number | null {
	if (!sample) return null;
	return Math.floor(projected(sample, now) - RESOLUTION_MARGIN_MS);
}

function projected(s: { serverMs: number; receivedAt: number }, now: number): number {
	return s.serverMs + Math.max(0, now - s.receivedAt);
}

/** Test seam: forget the sample. */
export function __resetServerClockForTests(): void {
	sample = null;
}
