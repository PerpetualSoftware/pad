import { describe, it, expect, vi, afterEach } from 'vitest';
import { api, PadApiError } from './client';

// TASK-4845: the settings page preflights the workspace bundle export so a
// 422 export_too_large is shown in the page instead of a failed download.
// The preflight must never read the body of a 2xx: the bundle carries the
// attachment blobs and can be gigabytes.

afterEach(() => {
	vi.unstubAllGlobals();
	vi.restoreAllMocks();
});

describe('api.workspaces.exportUrl', () => {
	it('asks for the tar bundle and keeps version history by default', () => {
		const url = api.workspaces.exportUrl('acme');
		expect(url).toBe('/api/v1/workspaces/acme/export?format=tar');
		expect(url).not.toContain('include_versions');
	});

	it('keeps history when includeVersions is true', () => {
		expect(api.workspaces.exportUrl('acme', { includeVersions: true })).not.toContain('include_versions');
	});

	it('adds include_versions=false when history is left out', () => {
		const url = api.workspaces.exportUrl('acme', { includeVersions: false });
		const q = new URLSearchParams(url.split('?')[1]);
		expect(q.get('format')).toBe('tar');
		expect(q.get('include_versions')).toBe('false');
	});

	it('encodes the slug', () => {
		expect(api.workspaces.exportUrl('a b/c')).toBe('/api/v1/workspaces/a%20b%2Fc/export?format=tar');
	});
});

function stubFetch(resp: Record<string, unknown>) {
	const fetchMock = vi.fn(async (_url: string, _init?: RequestInit) => resp);
	vi.stubGlobal('fetch', fetchMock);
	return fetchMock;
}

describe('api.workspaces.preflightExport', () => {
	it('resolves on 200 after aborting, without reading the body', async () => {
		const abort = vi.spyOn(AbortController.prototype, 'abort');
		const text = vi.fn();
		const blob = vi.fn();
		const json = vi.fn();
		const arrayBuffer = vi.fn();
		const fetchMock = stubFetch({ status: 200, ok: true, text, blob, json, arrayBuffer, body: null });

		await expect(api.workspaces.preflightExport('acme', { includeVersions: false })).resolves.toBeUndefined();

		expect(fetchMock).toHaveBeenCalledTimes(1);
		const [url, init] = fetchMock.mock.calls[0];
		expect(url).toBe('/api/v1/workspaces/acme/export?format=tar&include_versions=false');
		expect(init?.signal).toBeInstanceOf(AbortSignal);
		expect(init?.signal?.aborted).toBe(true);
		expect(abort).toHaveBeenCalled();
		expect(text).not.toHaveBeenCalled();
		expect(blob).not.toHaveBeenCalled();
		expect(json).not.toHaveBeenCalled();
		expect(arrayBuffer).not.toHaveBeenCalled();
	});

	it('throws export_too_large with its details on a 422', async () => {
		const details = { size_reached: 67_200_000, limit_bytes: 67_108_864, include_versions: true };
		stubFetch({
			status: 422,
			ok: false,
			json: async () => ({ error: { code: 'export_too_large', message: 'server words', details } })
		});
		const err = await api.workspaces.preflightExport('acme').catch((e) => e);
		expect(err).toBeInstanceOf(PadApiError);
		expect(err.code).toBe('export_too_large');
		expect(err.message).toBe('server words');
		expect(err.details).toEqual(details);
	});

	it('throws a generic PadApiError on a 500 with no JSON envelope', async () => {
		stubFetch({ status: 500, ok: false, json: async () => { throw new SyntaxError('not json'); } });
		const err = await api.workspaces.preflightExport('acme').catch((e) => e);
		expect(err).toBeInstanceOf(PadApiError);
		expect(err.code).toBe('export_failed');
		expect(err.message).toBe('Export failed (status 500). Try again.');
	});

	it('throws a network PadApiError when fetch rejects', async () => {
		vi.stubGlobal('fetch', vi.fn(async () => { throw new TypeError('Failed to fetch'); }));
		const err = await api.workspaces.preflightExport('acme').catch((e) => e);
		expect(err).toBeInstanceOf(PadApiError);
		expect(err.code).toBe('network_error');
	});

	it('throws export_cancelled when the caller aborts mid-fetch', async () => {
		let seen: AbortSignal | undefined;
		vi.stubGlobal(
			'fetch',
			vi.fn(
				(_url: string, init?: RequestInit) =>
					new Promise((_resolve, reject) => {
						seen = init?.signal ?? undefined;
						seen?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')));
					})
			)
		);
		const caller = new AbortController();
		const p = api.workspaces.preflightExport('acme', {}, caller.signal).catch((e) => e);
		await Promise.resolve();
		expect(seen?.aborted).toBe(false);
		caller.abort();
		const err = await p;
		expect(seen?.aborted).toBe(true);
		expect(err).toBeInstanceOf(PadApiError);
		expect(err.code).toBe('export_cancelled');
		expect(err.message).toBe('Export cancelled.');
	});

	it('throws export_cancelled at once for an already-aborted signal', async () => {
		let seen: AbortSignal | undefined;
		vi.stubGlobal(
			'fetch',
			vi.fn(
				(_url: string, init?: RequestInit) =>
					new Promise((_resolve, reject) => {
						seen = init?.signal ?? undefined;
						if (seen?.aborted) reject(new DOMException('aborted', 'AbortError'));
						seen?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')));
					})
			)
		);
		const caller = new AbortController();
		caller.abort();
		const err = await api.workspaces.preflightExport('acme', {}, caller.signal).catch((e) => e);
		expect(seen === undefined || seen.aborted).toBe(true);
		expect(err).toBeInstanceOf(PadApiError);
		expect(err.code).toBe('export_cancelled');
	});

	it('removes its abort listener from the caller signal once settled', async () => {
		stubFetch({ status: 200, ok: true, body: null });
		const caller = new AbortController();
		const add = vi.spyOn(caller.signal, 'addEventListener');
		const remove = vi.spyOn(caller.signal, 'removeEventListener');
		await api.workspaces.preflightExport('acme', {}, caller.signal);
		expect(add).toHaveBeenCalledTimes(1);
		expect(remove).toHaveBeenCalledWith('abort', add.mock.calls[0][1]);
	});

	it('throws unauthorized on a 401', async () => {
		stubFetch({ status: 401, ok: false, json: async () => ({}) });
		const err = await api.workspaces.preflightExport('acme').catch((e) => e);
		expect(err).toBeInstanceOf(PadApiError);
		expect(err.code).toBe('unauthorized');
	});
});
