// BUG-3211 — `request()` abandons an attempt the server never answers.
//
// Before this, `request()` passed no signal and set no timeout, so a request
// the server accepted but never answered was awaited indefinitely, and every
// gate held across the await held with it. The fetch doubles below never
// settle on their own: they settle only when the signal they were handed is
// aborted, which is exactly what a real hung connection does.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, setRequestTimeoutForTests, PadApiError } from './client';

const T = 40;

/** A fetch whose promise settles only if its signal aborts. */
function hungFetch() {
	const seen: AbortSignal[] = [];
	const fn = vi.fn((_url: string, init?: RequestInit) => {
		const signal = init?.signal as AbortSignal | undefined;
		if (signal) seen.push(signal);
		// No signal means nothing can ever abort it: it hangs, as a real hung
		// request would. (Throwing here instead turned a missing signal into an
		// instant rejection, and a mutant dropping the bound passed.)
		return new Promise<Response>((_resolve, reject) => {
			signal?.addEventListener('abort', () => reject(signal.reason), { once: true });
		});
	});
	return { fn, seen };
}

beforeEach(() => {
	vi.unstubAllGlobals();
	setRequestTimeoutForTests(T);
});
afterEach(() => {
	vi.unstubAllGlobals();
	setRequestTimeoutForTests();
});

describe('request() timeout (BUG-3211)', () => {
	it('a hung GET rejects with request_timeout instead of waiting forever', async () => {
		vi.stubGlobal('fetch', hungFetch().fn);
		const err = await api.workspaces.get('ws').catch((e) => e);
		expect(err).toBeInstanceOf(PadApiError);
		expect(err.code).toBe('request_timeout');
		expect(err.message).toBe('The server did not respond in time. Please try again.');
	});

	it('a hung WRITE says the change may still have been saved, and is not retried', async () => {
		const { fn } = hungFetch();
		vi.stubGlobal('fetch', fn);
		const err = await api.workspaces.create({ name: 'x' } as never).catch((e) => e);
		expect(err.code).toBe('request_timeout');
		expect(err.message).toContain('may still have been saved');
		expect(fn).toHaveBeenCalledTimes(1);
	});

	it('covers the BODY too: headers that arrive and a body that never does', async () => {
		vi.stubGlobal(
			'fetch',
			vi.fn(async (_url: string, init?: RequestInit) => {
				const signal = init?.signal as AbortSignal;
				return {
					status: 200,
					ok: true,
					headers: new Headers(),
					json: () =>
						new Promise((_resolve, reject) => {
							signal.addEventListener('abort', () => reject(signal.reason), { once: true });
						})
				} as unknown as Response;
			})
		);
		const err = await api.workspaces.get('ws').catch((e) => e);
		expect(err.code).toBe('request_timeout');
	});

	it("a CALLER's abort stays the caller's: an AbortError, not request_timeout", async () => {
		vi.stubGlobal('fetch', hungFetch().fn);
		const ctl = new AbortController();
		const p = api.items.copyPreflight('ws', 'item', {} as never, { signal: ctl.signal }).catch((e) => e);
		ctl.abort();
		const err = await p;
		expect(err).not.toBeInstanceOf(PadApiError);
		expect((err as DOMException).name).toBe('AbortError');
	});

	it('the direct session fetch (deduped by authStore.load) is under the same deadline', async () => {
		vi.stubGlobal('fetch', hungFetch().fn);
		const err = await api.auth.session().catch((e) => e);
		expect(err.code).toBe('request_timeout');
	});

	it('billing checkout (held under checkoutInProgress) times out as a WRITE', async () => {
		vi.stubGlobal('fetch', hungFetch().fn);
		const err = await api.billing.createCheckoutSession().catch((e) => e);
		expect(err.code).toBe('request_timeout');
		expect(err.message).toContain('may still have been saved');
	});

	it('transform has its OWN, longer bound: still pending past the default', async () => {
		vi.stubGlobal('fetch', hungFetch().fn);
		let settled = false;
		void api.attachments.transform('ws', 'a1', {} as never).then(
			() => (settled = true),
			() => (settled = true)
		);
		await new Promise((r) => setTimeout(r, T * 3));
		expect(settled, 'the 30s-class default does not apply to transform').toBe(false);
	});

	it('a hung metadata HEAD settles as transient and is evicted, so the next lookup asks again', async () => {
		const { fetchAttachmentMetadata, clearAttachmentMetadataCache } = await import(
			'$lib/components/editor/attachment-metadata'
		);
		clearAttachmentMetadataCache();
		const { fn } = hungFetch();
		vi.stubGlobal('fetch', fn);
		const url = (id: string) => `/api/v1/workspaces/ws/attachments/${id}`;
		await expect(fetchAttachmentMetadata('ws', 'a1', url, { timeoutMs: T })).resolves.toEqual({ status: 'transient' });
		await new Promise((r) => setTimeout(r, 0));
		void fetchAttachmentMetadata('ws', 'a1', url, { timeoutMs: T });
		expect(fn, 'the rejected lookup was not handed to the next caller').toHaveBeenCalledTimes(2);
	});

	it('a request that answers in time is not aborted afterwards (the timer is cleared)', async () => {
		const { seen } = hungFetch();
		vi.stubGlobal(
			'fetch',
			vi.fn(async (_url: string, init?: RequestInit) => {
				seen.push(init?.signal as AbortSignal);
				return { status: 200, ok: true, headers: new Headers(), json: async () => ({ slug: 'ws' }) } as unknown as Response;
			})
		);
		await expect(api.workspaces.get('ws')).resolves.toEqual({ slug: 'ws' });
		await new Promise((r) => setTimeout(r, T * 3));
		expect(seen[0].aborted).toBe(false);
	});
});

describe('the remaining direct fetches are bounded (BUG-3216)', () => {
	it('the share view', async () => {
		vi.stubGlobal('fetch', hungFetch().fn);
		const err = await api.share.get('tok').catch((e) => e);
		expect(err.code).toBe('request_timeout');
	});
	// adminFetch's leg is in stores/adminFetchTimeout.svelte.test.ts: the admin
	// store uses runes, which only the jsdom project compiles.
});
