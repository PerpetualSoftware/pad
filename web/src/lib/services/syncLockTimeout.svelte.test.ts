import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

/**
 * BUG-3211 — a hung /changes no longer holds the sync lock for ever.
 *
 * `triggerSync` holds `syncing` across its await, and a sync_required that
 * arrives meanwhile is only DEFERRED behind it. So a /changes the server never
 * answered used to stop the tab syncing for good: every later trigger saw
 * `syncing` and parked itself behind a request that would never settle.
 *
 * The REAL API client and the REAL sync service; only the network is modelled,
 * as a fetch that settles only when its signal aborts — what a half-open
 * connection does. The lock is released by the client's timeout reaching the
 * service's `finally`, which is the claim: nothing here mocks the release.
 */

const T = 40;
let healthy = true;
const urls: string[] = [];

function changesBody() {
	return { updated: [], deleted: [], server_time: 10_000, collections_changed: false };
}

beforeEach(() => {
	healthy = true;
	urls.length = 0;
	vi.resetModules();
	vi.stubGlobal(
		'fetch',
		vi.fn((url: string, init?: RequestInit) => {
			urls.push(url);
			const signal = init?.signal as AbortSignal | undefined;
			if (!healthy) {
				return new Promise<Response>((_resolve, reject) => {
					// No signal: nothing can abort it, so it hangs like a real hung request.
					signal?.addEventListener('abort', () => reject(signal.reason), { once: true });
				});
			}
			return Promise.resolve({
				status: 200,
				ok: true,
				headers: new Headers({ Date: new Date(10_000).toUTCString() }),
				json: async () => changesBody()
			} as unknown as Response);
		})
	);
});

afterEach(async () => {
	const client = await import('$lib/api/client');
	client.setRequestTimeoutForTests();
	vi.unstubAllGlobals();
});

describe('the sync lock survives a request the server never answers (BUG-3211)', () => {
	it('a hung /changes times out, releases `syncing`, and the next trigger syncs', async () => {
		const client = await import('$lib/api/client');
		client.setRequestTimeoutForTests(T);
		const { syncService } = await import('./sync.svelte');
		await syncService.setWorkspace('ws');
		syncService.markSynced(5_000);

		healthy = false;
		const before = urls.length;
		await syncService.triggerSync();
		expect(urls.length, 'the hung /changes was actually sent').toBe(before + 1);
		expect(syncService.syncing, 'the lock is released once the request times out').toBe(false);

		healthy = true;
		const seen: string[] = [];
		const off = syncService.onSync((r) => {
			seen.push((r as { type: string }).type);
		});
		await syncService.triggerSync();
		off();
		expect(urls.length, 'the next trigger issued a NEW request instead of parking').toBe(before + 2);
		// The modelled server has no changes, so a completed sync reports that.
		expect(seen, 'that request completed and its result was delivered').toEqual(['caught_up']);
	});
});
