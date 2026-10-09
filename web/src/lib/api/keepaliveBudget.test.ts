// BUG-3522: a keepalive request over the browser's 64 KiB cap is refused
// outright, so the client asks for keepalive only when the body fits a budget
// and otherwise sends an ordinary request.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { KEEPALIVE_BODY_BUDGET, fitsKeepaliveBudget, keepaliveBytesInFlight, reserveKeepalive } from './keepaliveBudget';
import { api } from './client';

describe('the keepalive budget (BUG-3522)', () => {
	it('sits under the 64 KiB cap the Fetch spec sets', () => {
		expect(KEEPALIVE_BODY_BUDGET).toBeLessThan(64 * 1024);
	});

	it('counts UTF-8 bytes, not string length', () => {
		const ascii = 'a'.repeat(KEEPALIVE_BODY_BUDGET);
		expect(fitsKeepaliveBudget(ascii)).toBe(true);
		expect(fitsKeepaliveBudget(ascii + 'a')).toBe(false);
		// '€' is one UTF-16 unit and three UTF-8 bytes: well under the budget by
		// length, well over it by bytes.
		const euros = '€'.repeat(Math.ceil(KEEPALIVE_BODY_BUDGET / 3) + 1);
		expect(euros.length).toBeLessThan(KEEPALIVE_BODY_BUDGET);
		expect(fitsKeepaliveBudget(euros)).toBe(false);
	});

	it('keeps what the caller asked for when it fits, and says so when it drops it', () => {
		const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
		const small = reserveKeepalive(true, 'small', 'x');
		expect(small.keepalive).toBe(true);
		small.release();
		expect(reserveKeepalive(false, 'a'.repeat(KEEPALIVE_BODY_BUDGET + 1), 'x').keepalive).toBe(false);
		expect(reserveKeepalive(undefined, 'a'.repeat(KEEPALIVE_BODY_BUDGET + 1), 'x').keepalive).toBeUndefined();
		expect(warn).not.toHaveBeenCalled();
		expect(reserveKeepalive(true, 'a'.repeat(KEEPALIVE_BODY_BUDGET + 1), 'collab flush of x').keepalive).toBe(false);
		expect(warn).toHaveBeenCalledTimes(1);
		expect(String(warn.mock.calls[0][0])).toContain('collab flush of x');
		expect(keepaliveBytesInFlight()).toBe(0);
		warn.mockRestore();
	});

	it('counts every keepalive body in flight: two that fit alone can overflow together (codex r1)', () => {
		const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
		const half = 'a'.repeat(Math.floor(KEEPALIVE_BODY_BUDGET * 0.6));
		const first = reserveKeepalive(true, half, 'collab flush');
		expect(first.keepalive).toBe(true);
		// The raw saver's unload write, while the first is still in flight.
		const second = reserveKeepalive(true, half, 'raw save');
		expect(second.keepalive).toBe(false);
		expect(warn).toHaveBeenCalledTimes(1);
		// Once the first settles, the budget is back.
		first.release();
		first.release(); // idempotent
		expect(keepaliveBytesInFlight()).toBe(0);
		const third = reserveKeepalive(true, half, 'raw save');
		expect(third.keepalive).toBe(true);
		third.release();
		expect(keepaliveBytesInFlight()).toBe(0);
		warn.mockRestore();
	});
});

describe('the client sends what the budget decides', () => {
	let calls: RequestInit[] = [];
	beforeEach(() => {
		calls = [];
		vi.stubGlobal(
			'fetch',
			vi.fn(async (_url: string, init: RequestInit) => {
				calls.push(init);
				return { status: 200, ok: true, json: async () => ({}) };
			})
		);
		vi.spyOn(console, 'warn').mockImplementation(() => {});
	});
	afterEach(() => {
		vi.unstubAllGlobals();
		vi.restoreAllMocks();
	});

	const big = 'x'.repeat(87 * 1024); // the 86 KB plan body that was refused

	it('a teardown collab flush keeps keepalive for a small body and drops it for a large one', async () => {
		await api.items.flushCollabContent('ws', 'i1', 'small body', { keepalive: true });
		await api.items.flushCollabContent('ws', 'i1', big, { keepalive: true });
		expect(calls.map((c) => c.keepalive)).toEqual([true, false]);
		// The large one is still SENT, with its whole body: an ordinary request,
		// never nothing.
		expect(String(calls[1].body)).toContain(big);
	});

	it('a request releases its reservation when it settles, success or failure', async () => {
		await api.items.flushCollabContent('ws', 'i1', 'small body', { keepalive: true });
		expect(keepaliveBytesInFlight()).toBe(0);
		vi.stubGlobal('fetch', vi.fn(async () => { throw new TypeError('Failed to fetch'); }));
		await api.items.flushCollabContent('ws', 'i1', 'small body', { keepalive: true }).catch(() => {});
		expect(keepaliveBytesInFlight()).toBe(0);
	});

	it('the raw saver\'s unload PATCH does the same', async () => {
		await api.items.update('ws', 'i1', { content: 'small' }, { keepalive: true });
		await api.items.update('ws', 'i1', { content: big }, { keepalive: true });
		expect(calls.map((c) => c.keepalive)).toEqual([true, false]);
	});
});
