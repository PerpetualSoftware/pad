// Reconnect policy for the activity stream's EventSource (BUG-2733).
//
// The browser's own EventSource reconnect does two different things, measured
// in Chromium on BUG-2733's trail: a stream that ENDS or DROPS is retried on a
// flat 3s with no growth (every tab of a restarted server comes back in
// lockstep), and a connection REFUSED with a non-200 status (429
// sse_limit_exceeded, 503 subscription_failed) is never retried at all. The
// source goes CLOSED and the tab stays dark while its indicator says
// "reconnecting". sse.svelte.ts therefore owns the reconnect, and this module
// is the policy it follows.

/** First step of the ladder, matching the CLI monitor's padddBackoffBase. */
export const RECONNECT_BASE_MS = 5_000;
/** Ceiling of the ladder, matching the CLI monitor's padddBackoffCap. */
export const RECONNECT_CAP_MS = 5 * 60_000;

/**
 * How long to wait before reconnect `attempt` (1-based).
 *
 * The ladder is the CLI's: linear from 5s, capped at 5min. A server
 * Retry-After raises the floor and is never undercut.
 *
 * JITTER IS UPWARD ONLY: `d + rand·d/2`. A deterministic ladder brings every
 * tab of a restarted server back on the same tick, which is the herd this
 * exists to break. Jittering downward would let a client come back before the
 * Retry-After it was given.
 */
export function reconnectDelayMs(attempt: number, retryAfterMs: number | null, rand: () => number = Math.random): number {
	const step = Math.min(Math.max(attempt, 1) * RECONNECT_BASE_MS, RECONNECT_CAP_MS);
	const floor = Math.max(step, retryAfterMs ?? 0);
	return Math.round(floor + rand() * (floor / 2));
}

/**
 * Parse a Retry-After header value into milliseconds from `now`, in either of
 * its two forms: delta-seconds or an HTTP-date. Null for an absent, malformed
 * or already-past value, which the caller treats as "no floor". Mirrors the
 * CLI's parseRetryAfterSeconds.
 */
export function parseRetryAfterMs(value: string | null, now: number = Date.now()): number | null {
	if (!value) return null;
	const trimmed = value.trim();
	if (/^\d+$/.test(trimmed)) {
		const seconds = Number(trimmed);
		return seconds > 0 ? seconds * 1000 : null;
	}
	const at = Date.parse(trimmed);
	if (Number.isNaN(at)) return null;
	const ms = at - now;
	return ms > 0 ? ms : null;
}

/**
 * What a refusal probe learned. `unauthorized` is a 401 or 403: an expired
 * session or a lost membership, which refuses the stream exactly as a limit
 * does and must NOT be retried (before the owned reconnect such a tab simply
 * stayed closed). Anything else is `retry`, with the Retry-After to honour if
 * the server gave one.
 */
export type RefusalProbe = { kind: 'unauthorized' } | { kind: 'retry'; retryAfterMs: number | null };

/**
 * Ask the stream endpoint why it refused, because EventSource cannot say: the
 * spec exposes neither the status nor the headers of a failed connection.
 *
 * Called ONLY after a refusal (the source went CLOSED at onerror), never for a
 * dropped stream, so it costs one extra request on a path that is already rare.
 * The body is aborted as soon as the headers arrive: a 200 here is a live SSE
 * stream holding an admission slot, and nothing should read it.
 *
 * A failed probe (network error) is a retry with no floor, and the ladder
 * alone decides: it says nothing about the credential.
 */
export async function probeRefusal(url: string, fetchFn: typeof fetch = fetch): Promise<RefusalProbe> {
	const controller = new AbortController();
	try {
		const res = await fetchFn(url, {
			method: 'GET',
			headers: { Accept: 'text/event-stream' },
			credentials: 'same-origin',
			signal: controller.signal
		});
		if (res.status === 401 || res.status === 403) {
			return { kind: 'unauthorized' };
		}
		if (res.status === 429 || res.status === 503) {
			return { kind: 'retry', retryAfterMs: parseRetryAfterMs(res.headers.get('Retry-After')) };
		}
		return { kind: 'retry', retryAfterMs: null };
	} catch {
		return { kind: 'retry', retryAfterMs: null };
	} finally {
		controller.abort();
	}
}
