// BUG-3522: a keepalive request over the browser's 64 KiB cap is refused
// outright, so the client asks for keepalive only when the body fits a budget
// and otherwise sends an ordinary request.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { KEEPALIVE_BODY_BUDGET, fitsKeepaliveBudget, keepaliveFor } from './keepaliveBudget';
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
		expect(keepaliveFor(true, 'small', 'x')).toBe(true);
		expect(keepaliveFor(false, 'a'.repeat(KEEPALIVE_BODY_BUDGET + 1), 'x')).toBe(false);
		expect(keepaliveFor(undefined, 'a'.repeat(KEEPALIVE_BODY_BUDGET + 1), 'x')).toBeUndefined();
		expect(warn).not.toHaveBeenCalled();
		expect(keepaliveFor(true, 'a'.repeat(KEEPALIVE_BODY_BUDGET + 1), 'collab flush of x')).toBe(false);
		expect(warn).toHaveBeenCalledTimes(1);
		expect(String(warn.mock.calls[0][0])).toContain('collab flush of x');
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

	it('the raw saver\'s unload PATCH does the same', async () => {
		await api.items.update('ws', 'i1', { content: 'small' }, { keepalive: true });
		await api.items.update('ws', 'i1', { content: big }, { keepalive: true });
		expect(calls.map((c) => c.keepalive)).toEqual([true, false]);
	});
});
