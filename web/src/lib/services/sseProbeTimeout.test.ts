// BUG-3211 — the refused-stream probe is bounded.
//
// `scheduleReconnect` holds `reconnectPending` until `probeRefusal` settles,
// and nothing else clears it. A probe whose headers never arrived therefore
// stopped the tab from ever reconnecting its stream. A timed-out probe is a
// `retry` with no floor — what a network error already means there — which
// releases the flag through the path that already handles a failed probe.
import { describe, expect, it, vi } from 'vitest';
import { probeRefusal, PROBE_TIMEOUT_MS } from './sseReconnect';

function hungFetch() {
	return vi.fn((_url: string, init?: RequestInit) => {
		const signal = init?.signal as AbortSignal | undefined;
		return new Promise<Response>((_resolve, reject) => {
			// No signal: nothing can abort it, so it hangs like a real hung request.
			signal?.addEventListener('abort', () => reject(signal.reason), { once: true });
		});
	}) as unknown as typeof fetch;
}

describe('probeRefusal is bounded (BUG-3211)', () => {
	it('a probe whose headers never arrive settles as a retry with no floor', async () => {
		await expect(probeRefusal('/api/v1/events?workspace=ws', hungFetch(), 30)).resolves.toEqual({
			kind: 'retry',
			retryAfterMs: null
		});
	});

	it('defaults to a finite bound, well under the API timeout', () => {
		expect(PROBE_TIMEOUT_MS).toBe(10_000);
	});
});
