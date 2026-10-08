// TASK-2202 — a request that cannot reach the server says so.
//
// A fetch that rejects before any response (offline, a dead route) used to
// surface as a raw TypeError, so every call site showed its generic failure
// text and nothing said the server was unreachable. It is now a PadApiError
// with its own code, and the registered handler fires once per such request.
// Aborts keep their own paths, and an error thrown AFTER a response exists is
// not relabelled.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
	api,
	PadApiError,
	isNetworkUnreachable,
	setNetworkUnreachableHandler,
	setRequestTimeoutForTests
} from './client';

let notified = 0;

beforeEach(() => {
	vi.unstubAllGlobals();
	notified = 0;
	setNetworkUnreachableHandler(() => {
		notified++;
	});
});
afterEach(() => {
	vi.unstubAllGlobals();
	setNetworkUnreachableHandler(null);
	setRequestTimeoutForTests();
});

describe('network_unreachable (TASK-2202)', () => {
	it('a GET whose fetch rejects is network_unreachable, and the handler fires once', async () => {
		vi.stubGlobal('fetch', vi.fn(async () => Promise.reject(new TypeError('Failed to fetch'))));
		const err = await api.workspaces.get('ws').catch((e) => e);
		expect(err).toBeInstanceOf(PadApiError);
		expect(err.code).toBe('network_unreachable');
		expect(isNetworkUnreachable(err)).toBe(true);
		expect(err.message).toBe("Can't reach the server. Check your connection and try again.");
		expect(notified).toBe(1);
	});

	it("a WRITE's message does not claim the change was lost", async () => {
		const fetchMock = vi.fn(async () => Promise.reject(new TypeError('Failed to fetch')));
		vi.stubGlobal('fetch', fetchMock);
		const err = await api.workspaces.create({ name: 'x' } as never).catch((e) => e);
		expect(err.code).toBe('network_unreachable');
		expect(err.message).toContain('may not have been saved');
		expect(fetchMock).toHaveBeenCalledTimes(1);
	});

	it("a caller's abort stays an AbortError and does not report an outage", async () => {
		vi.stubGlobal(
			'fetch',
			vi.fn(
				(_url: string, init?: RequestInit) =>
					new Promise<Response>((_resolve, reject) => {
						const signal = init?.signal as AbortSignal;
						signal.addEventListener('abort', () => reject(signal.reason), { once: true });
					})
			)
		);
		const ctl = new AbortController();
		const p = api.items.copyPreflight('ws', 'item', {} as never, { signal: ctl.signal }).catch((e) => e);
		ctl.abort();
		const err = await p;
		expect(err).not.toBeInstanceOf(PadApiError);
		expect((err as DOMException).name).toBe('AbortError');
		expect(notified).toBe(0);
	});

	it('a timeout stays request_timeout and does not report an outage', async () => {
		setRequestTimeoutForTests(30);
		vi.stubGlobal(
			'fetch',
			vi.fn(
				(_url: string, init?: RequestInit) =>
					new Promise<Response>((_resolve, reject) => {
						const signal = init?.signal as AbortSignal;
						signal.addEventListener('abort', () => reject(new TypeError('Failed to fetch')), { once: true });
					})
			)
		);
		const err = await api.workspaces.get('ws').catch((e) => e);
		expect(err.code).toBe('request_timeout');
		expect(notified).toBe(0);
	});

	it('a TypeError after a response arrived is not relabelled as an outage', async () => {
		vi.stubGlobal(
			'fetch',
			vi.fn(
				async () =>
					({
						status: 200,
						ok: true,
						headers: new Headers(),
						json: async () => {
							throw new TypeError('body went wrong');
						}
					}) as unknown as Response
			)
		);
		const err = await api.workspaces.get('ws').catch((e) => e);
		expect(isNetworkUnreachable(err)).toBe(false);
		expect(notified).toBe(0);
	});
});
