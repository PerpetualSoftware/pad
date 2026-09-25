// BUG-3216 — the admin console's direct fetch is under the API deadline.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setRequestTimeoutForTests } from '$lib/api/client';
import { adminFetch, adminPost } from './admin.svelte';

function hungFetch() {
	return vi.fn((_url: string, init?: RequestInit) => {
		const signal = init?.signal ?? undefined;
		// No signal: nothing can abort it, so it hangs like a real hung request.
		return new Promise<Response>((_resolve, reject) => {
			signal?.addEventListener('abort', () => reject(signal.reason), { once: true });
		});
	});
}

beforeEach(() => setRequestTimeoutForTests(40));
afterEach(() => {
	setRequestTimeoutForTests();
	vi.unstubAllGlobals();
});

describe('adminFetch is bounded (BUG-3216)', () => {
	it('a hung GET times out as a read', async () => {
		vi.stubGlobal('fetch', hungFetch());
		const err = await adminFetch('/admin/stats').catch((e) => e);
		expect(err.code).toBe('request_timeout');
		expect(err.message).toBe('The server did not respond in time. Please try again.');
	});

	it('a hung POST times out as a write that may have landed', async () => {
		vi.stubGlobal('fetch', hungFetch());
		const err = await adminPost('/admin/x', {}).catch((e) => e);
		expect(err.code).toBe('request_timeout');
		expect(err.message).toContain('may still have been saved');
	});
});
