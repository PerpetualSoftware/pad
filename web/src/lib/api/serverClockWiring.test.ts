import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from './client';
import { __resetServerClockForTests, estimateServerNow } from './serverClock';

/**
 * BUG-3207 — WIRING (CONVE-19): the API client's `request` feeds every
 * response's `Date` header to the server-clock estimate. serverClock.test.ts
 * vouches for the estimate; this vouches for the binding that makes it
 * non-null in a real tab.
 */

afterEach(() => {
	__resetServerClockForTests();
	vi.unstubAllGlobals();
	vi.restoreAllMocks();
});

function respond(status: number, date: string | null, body: unknown) {
	const headers = new Headers({ 'Content-Type': 'application/json' });
	if (date) headers.set('Date', date);
	return new Response(JSON.stringify(body), { status, headers });
}

describe('the API client records the server clock (BUG-3207)', () => {
	it('a response carrying Date seeds the estimate', async () => {
		const serverMs = Date.UTC(2026, 8, 25, 1, 2, 3);
		vi.stubGlobal(
			'fetch',
			vi.fn(async () =>
				respond(200, new Date(serverMs).toUTCString(), {
					updated: [],
					deleted: [],
					server_time: serverMs,
					collections_changed: false,
				}),
			),
		);
		expect(estimateServerNow()).toBeNull();
		await api.changes.since('ws', 0);
		const estimate = estimateServerNow();
		expect(estimate).not.toBeNull();
		expect(estimate!).toBeLessThanOrEqual(serverMs);
	});

	it('an error response is a clock reading too', async () => {
		const serverMs = Date.UTC(2026, 8, 25, 1, 2, 3);
		vi.stubGlobal(
			'fetch',
			vi.fn(async () => respond(500, new Date(serverMs).toUTCString(), { error: { code: 'internal', message: 'x' } })),
		);
		await api.changes.since('ws', 0).catch(() => {});
		expect(estimateServerNow()).not.toBeNull();
	});
});
