import { afterEach, describe, expect, it, vi } from 'vitest';
import {
	RESOLUTION_MARGIN_MS,
	__resetServerClockForTests,
	estimateServerNow,
	noteServerDate,
} from './serverClock';

/**
 * BUG-3207 — the server-clock estimate the sync cursor is stamped from. Its one
 * property is that it never runs AHEAD of the server: an early cursor
 * re-delivers (a duplicate), a late one skips (a miss nothing recovers).
 */

const httpDate = (ms: number) => new Date(ms).toUTCString();

afterEach(() => {
	__resetServerClockForTests();
	vi.restoreAllMocks();
});

describe('serverClock (BUG-3207)', () => {
	it('is null until a response carried a parseable Date', () => {
		expect(estimateServerNow(0)).toBeNull();
		noteServerDate(null, 0);
		noteServerDate('not a date', 0);
		expect(estimateServerNow(0)).toBeNull();
	});

	it('never runs ahead of the true server clock, across truncation, latency and elapsed time', () => {
		// The server writes the response at trueSend (ms precision); the header
		// carries it truncated to the second; the client receives it `latency`
		// later on its monotonic clock, then asks `elapsed` after that.
		for (const trueSend of [10_000, 10_999, 1_790_000_123_456]) {
			for (const latency of [0, 1, 250, 5_000]) {
				for (const elapsed of [0, 17, 60_000, 3_600_000]) {
					__resetServerClockForTests();
					const receivedAt = 1_000;
					noteServerDate(httpDate(trueSend), receivedAt);
					const estimate = estimateServerNow(receivedAt + elapsed)!;
					const trueNow = trueSend + latency + elapsed;
					expect(estimate, `send ${trueSend} latency ${latency} elapsed ${elapsed}`).toBeLessThanOrEqual(trueNow);
					// ...and not absurdly early: within the header's second, the
					// margin, and the unobservable latency.
					expect(trueNow - estimate).toBeLessThan(1_000 + RESOLUTION_MARGIN_MS + latency + 1);
				}
			}
		}
	});

	it('keeps the sample that projects newest, not the one received last', () => {
		// A newer Date received later wins.
		noteServerDate(httpDate(10_000), 0);
		noteServerDate(httpDate(20_000), 1_000);
		expect(estimateServerNow(1_000)).toBe(20_000 - RESOLUTION_MARGIN_MS);
		// An OLDER Date arriving later (a slow response) does not drag it back.
		noteServerDate(httpDate(15_000), 2_000);
		expect(estimateServerNow(2_000)).toBe(21_000 - RESOLUTION_MARGIN_MS);
	});

	it('ignores the client wall clock entirely', () => {
		noteServerDate(httpDate(10_000), 0);
		const before = estimateServerNow(500);
		vi.spyOn(Date, 'now').mockReturnValue(10_000 + 86_400_000); // a day ahead
		expect(estimateServerNow(500)).toBe(before);
	});
});
