import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

/**
 * BUG-3211 — a hung bootstrap no longer holds the local index's dedup slot for
 * ever.
 *
 * `localIndex.bootstrap` dedups concurrent callers through its `inflight` map:
 * while a load is registered, every later caller is handed the SAME promise.
 * So a load whose request the server never answered used to leave every later
 * bootstrap of that workspace awaiting a promise that would never settle.
 *
 * Two things have to hold, and this pins both: the request times out (the
 * client's job), and the rejected promise is DROPPED from the map rather than
 * kept for later callers to inherit (the store's `finally`). The REAL client
 * and the REAL store; the persistence module is mocked only because jsdom has
 * no IndexedDB, and the network is a fetch that settles only when aborted.
 */

const persistence = vi.hoisted(() => ({
	hydrate: vi.fn(async () => ({
		items: [],
		cursor: null,
		includesUnparentedMetadata: null,
		accessEpoch: null,
		durableRead: true,
		retags: {},
	})),
	persistDelta: vi.fn(async () => undefined),
	persistRemovals: vi.fn(async () => undefined),
	persistReplace: vi.fn(async () => true),
	persistAccessEpoch: vi.fn(async () => undefined),
	persistRetag: vi.fn(async () => undefined),
	persistUpserts: vi.fn(async () => undefined),
	wipe: vi.fn(async () => undefined),
}));
vi.mock('./localIndexPersistence', () => persistence);

const { localIndex } = await import('./localIndex.svelte');
const client = await import('$lib/api/client');

const T = 40;
const ws = 'bootstrap-timeout';
let healthy = false;
const urls: string[] = [];

beforeEach(() => {
	healthy = false;
	urls.length = 0;
	client.setRequestTimeoutForTests(T);
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
				headers: new Headers(),
				json: async () => ({ items: [], cursor: '1', includes_unparented_metadata: true, access_epoch: 'e1' }),
			} as unknown as Response);
		})
	);
});

afterEach(() => {
	client.setRequestTimeoutForTests();
	vi.unstubAllGlobals();
	localIndex.reset(ws);
});

describe('the bootstrap dedup slot survives a request the server never answers (BUG-3211)', () => {
	it('a hung load rejects, is dropped from the dedup map, and the next bootstrap issues a new request', async () => {
		const first = localIndex.bootstrap(ws, { userId: null }).then(
			() => 'resolved',
			(e) => e
		);
		// A second caller DURING the hang is deduped onto the same promise.
		const joined = localIndex.bootstrap(ws, { userId: null }).then(
			() => 'resolved',
			(e) => e
		);
		const [a, b] = await Promise.all([first, joined]);
		expect(a?.code, 'the hung load timed out').toBe('request_timeout');
		expect(b?.code, 'the joined caller got the same outcome').toBe('request_timeout');
		const sentWhileHung = urls.length;
		expect(sentWhileHung, 'one request for the two deduped callers').toBe(1);

		healthy = true;
		await localIndex.bootstrap(ws, { userId: null });
		expect(urls.length, 'the next bootstrap sent its OWN request rather than inheriting the rejection').toBe(
			sentWhileHung + 1
		);
		expect(localIndex.bootstrapStateFor(ws)).toBe('ready');
	});
});
