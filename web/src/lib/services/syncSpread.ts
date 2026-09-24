/**
 * How long a tab may wait before answering a server `sync_required` (BUG-2761).
 *
 * The server sends `sync_required` mid-stream when a subscriber's replay
 * coverage ended — and the causes that end it are FLEET-WIDE: a Redis failover,
 * an ID-space epoch change, a subscription's idle_timeout after a network event
 * wedged many routes at once. Every subscriber of every affected workspace is
 * told in the same instant, and the streams stay open, so the SSE admission
 * limits never see it. Answered at once, every tab's resync (`/changes`, then
 * the layout's `/items-changes` reconcile, then whatever the route reloads)
 * lands on the database together.
 *
 * Each tab therefore waits a uniform random delay in [0, SYNC_REQUIRED_SPREAD_MS)
 * before dispatching. Drawn per TAB, not per browser, so N tabs of one user
 * spread as well as N users.
 *
 * WHY 5 SECONDS. The trade is peak request rate against added staleness: N tabs
 * answering over a window W arrive at about N·k/W requests a second (k requests
 * per resync, at least two), and every tab's view may be up to W later than it
 * would have been. 5s is already the latency the server itself accepts for this
 * signal: `midStreamGapCooldown` (internal/server/stream_gap_announcer.go) tells
 * a slow subscriber of a new gap at most once per 5s, so a client adding up to
 * 5s more is within the staleness the protocol already budgets, and it turns an
 * instant into a spread of 5s (mean 2.5s). The server-sized version — spreading
 * a fleet-wide event wider and a single stream's gap not at all — is IDEA-3205.
 *
 * ONLY `sync_required` (lead ruling on BUG-2761's trail). `items_bulk_updated`
 * and a collection change that rewrote items are targeted freshness: the page
 * that is open wants them now, and delaying them is how an open page learns
 * late (BUG-3198). They dispatch immediately and are never spread.
 */
export const SYNC_REQUIRED_SPREAD_MS = 5000;

export function syncRequiredSpreadDelayMs(rand: () => number = Math.random): number {
	return Math.floor(rand() * SYNC_REQUIRED_SPREAD_MS);
}
