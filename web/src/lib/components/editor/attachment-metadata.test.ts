import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import {
	fetchAttachmentMetadata,
	invalidateAttachmentMetadata,
	revalidateAttachmentMetadata,
	mimeToFormat
} from './attachment-metadata';

// PLAN-2392 DR-17. The three arms exist so a caller can tell "the row is
// gone" (latch the placeholder — editor undo must not resurrect a deleted
// attachment) apart from "the request didn't make it" (stay retryable).
// The old helper collapsed both into `null` AND cached it, which made a
// one-off blip permanently sticky for the page's lifetime.

const url = (id: string) => `/api/v1/workspaces/ws/attachments/${id}`;

/** A HEAD response with the headers the helper reads. */
function head(status: number, headers: Record<string, string> = {}): Response {
	return new Response(null, { status, headers });
}

let fetchMock: ReturnType<typeof vi.fn>;

// Each test uses a fresh uuid AND invalidates it, because the module-level
// promise cache is process-wide and deliberately outlives a single probe.
let counter = 0;
function freshUuid(): string {
	counter += 1;
	return `uuid-${counter}`;
}

beforeEach(() => {
	fetchMock = vi.fn();
	vi.stubGlobal('fetch', fetchMock);
});

afterEach(() => {
	vi.unstubAllGlobals();
});

describe('fetchAttachmentMetadata — result arms', () => {
	it('returns ok with the parsed MIME and size on 200', async () => {
		const uuid = freshUuid();
		fetchMock.mockResolvedValue(
			head(200, { 'content-type': 'image/png; charset=binary', 'content-length': '4096' })
		);

		const result = await fetchAttachmentMetadata('ws', uuid, url);

		expect(result).toEqual({ status: 'ok', mime: 'image/png', size: 4096 });
		// HEAD, not GET — a GET would pull the whole blob across the wire.
		expect(fetchMock).toHaveBeenCalledWith(url(uuid), {
			method: 'HEAD',
			credentials: 'same-origin'
		});
	});

	it('falls back to a zero size when content-length is absent or junk', async () => {
		const uuid = freshUuid();
		fetchMock.mockResolvedValue(head(200, { 'content-type': 'application/pdf' }));

		expect(await fetchAttachmentMetadata('ws', uuid, url)).toEqual({
			status: 'ok',
			mime: 'application/pdf',
			size: 0
		});
	});

	it('reports 404 as missing — the authoritative "row is gone" answer', async () => {
		const uuid = freshUuid();
		fetchMock.mockResolvedValue(head(404));

		expect(await fetchAttachmentMetadata('ws', uuid, url)).toEqual({ status: 'missing' });
	});

	it('reports a 500 as transient, not missing', async () => {
		const uuid = freshUuid();
		fetchMock.mockResolvedValue(head(500));

		expect(await fetchAttachmentMetadata('ws', uuid, url)).toEqual({ status: 'transient' });
	});

	it('reports a mid-session 403 as transient, not missing', async () => {
		const uuid = freshUuid();
		fetchMock.mockResolvedValue(head(403));

		expect(await fetchAttachmentMetadata('ws', uuid, url)).toEqual({ status: 'transient' });
	});

	it('reports a network throw as transient rather than rejecting', async () => {
		const uuid = freshUuid();
		fetchMock.mockRejectedValue(new TypeError('Failed to fetch'));

		expect(await fetchAttachmentMetadata('ws', uuid, url)).toEqual({ status: 'transient' });
	});
});

describe('fetchAttachmentMetadata — caching is per-arm', () => {
	it('caches an ok result for the page lifetime', async () => {
		const uuid = freshUuid();
		fetchMock.mockResolvedValue(head(200, { 'content-type': 'image/webp', 'content-length': '10' }));

		await fetchAttachmentMetadata('ws', uuid, url);
		await fetchAttachmentMetadata('ws', uuid, url);

		expect(fetchMock).toHaveBeenCalledTimes(1);
	});

	it('caches a missing result — deletion is durable, so stop asking', async () => {
		const uuid = freshUuid();
		fetchMock.mockResolvedValue(head(404));

		expect(await fetchAttachmentMetadata('ws', uuid, url)).toEqual({ status: 'missing' });
		expect(await fetchAttachmentMetadata('ws', uuid, url)).toEqual({ status: 'missing' });

		expect(fetchMock).toHaveBeenCalledTimes(1);
	});

	it('does NOT cache a transient failure — a retry re-issues the HEAD', async () => {
		const uuid = freshUuid();
		fetchMock.mockResolvedValueOnce(head(503));

		expect(await fetchAttachmentMetadata('ws', uuid, url)).toEqual({ status: 'transient' });

		// ...and the row was fine all along: the second probe must reach the
		// network and see that, rather than replaying the cached failure.
		fetchMock.mockResolvedValueOnce(
			head(200, { 'content-type': 'image/jpeg', 'content-length': '7' })
		);
		expect(await fetchAttachmentMetadata('ws', uuid, url)).toEqual({
			status: 'ok',
			mime: 'image/jpeg',
			size: 7
		});
		expect(fetchMock).toHaveBeenCalledTimes(2);
	});

	it('does NOT cache a network throw either', async () => {
		const uuid = freshUuid();
		fetchMock.mockRejectedValueOnce(new TypeError('offline'));

		await fetchAttachmentMetadata('ws', uuid, url);

		fetchMock.mockResolvedValueOnce(head(404));
		expect(await fetchAttachmentMetadata('ws', uuid, url)).toEqual({ status: 'missing' });
		expect(fetchMock).toHaveBeenCalledTimes(2);
	});

	it('still shares one in-flight HEAD between concurrent callers that fail', async () => {
		const uuid = freshUuid();
		fetchMock.mockResolvedValue(head(500));

		const [a, b] = await Promise.all([
			fetchAttachmentMetadata('ws', uuid, url),
			fetchAttachmentMetadata('ws', uuid, url)
		]);

		expect(a).toEqual({ status: 'transient' });
		expect(b).toEqual({ status: 'transient' });
		// Eviction happens when the promise SETTLES, so dedupe survives.
		expect(fetchMock).toHaveBeenCalledTimes(1);
	});

	it('keys the cache by workspace as well as uuid', async () => {
		const uuid = freshUuid();
		fetchMock.mockResolvedValue(head(200, { 'content-type': 'image/gif', 'content-length': '1' }));

		await fetchAttachmentMetadata('ws-a', uuid, url);
		await fetchAttachmentMetadata('ws-b', uuid, url);

		expect(fetchMock).toHaveBeenCalledTimes(2);
	});

	it('invalidate drops a cached entry so the next call refetches', async () => {
		const uuid = freshUuid();
		fetchMock.mockResolvedValue(head(200, { 'content-type': 'image/png', 'content-length': '2' }));

		await fetchAttachmentMetadata('ws', uuid, url);
		invalidateAttachmentMetadata('ws', uuid);
		await fetchAttachmentMetadata('ws', uuid, url);

		expect(fetchMock).toHaveBeenCalledTimes(2);
	});

	it('transient eviction cannot delete a newer entry installed by an invalidate race', async () => {
		const uuid = freshUuid();
		let releaseFirst!: (r: Response) => void;
		fetchMock.mockImplementationOnce(
			() => new Promise<Response>((resolve) => (releaseFirst = resolve))
		);

		const slow = fetchAttachmentMetadata('ws', uuid, url);

		// A delete/transform lands, the entry is invalidated, and a fresh
		// probe caches a good result — all before the first HEAD settles.
		invalidateAttachmentMetadata('ws', uuid);
		fetchMock.mockResolvedValueOnce(
			head(200, { 'content-type': 'image/avif', 'content-length': '3' })
		);
		await fetchAttachmentMetadata('ws', uuid, url);

		releaseFirst(head(500));
		expect(await slow).toEqual({ status: 'transient' });

		// The newer, good entry survived the older promise's eviction.
		expect(await fetchAttachmentMetadata('ws', uuid, url)).toEqual({
			status: 'ok',
			mime: 'image/avif',
			size: 3
		});
		expect(fetchMock).toHaveBeenCalledTimes(2);
	});
});

describe('revalidateAttachmentMetadata — existence probes ignore the cache', () => {
	// The orchestrator's Codex pass on TASK-2420 caught this: `ok` is cached
	// for the page lifetime (correctly — MIME and size are durable facts about
	// a content-addressed row), but that makes the cache structurally unable
	// to answer "is this row still there?". An <img> whose load just failed
	// holds evidence that the cached observation is stale.
	it('re-issues the HEAD and observes a 404 that a cached ok would have hidden', async () => {
		const uuid = freshUuid();
		fetchMock.mockResolvedValueOnce(
			head(200, { 'content-type': 'image/png', 'content-length': '10' })
		);
		expect(await fetchAttachmentMetadata('ws', uuid, url)).toEqual({
			status: 'ok',
			mime: 'image/png',
			size: 10
		});

		// The row is deleted by someone else; the cached `ok` still says live.
		expect(await fetchAttachmentMetadata('ws', uuid, url)).toMatchObject({ status: 'ok' });
		expect(fetchMock).toHaveBeenCalledTimes(1);

		fetchMock.mockResolvedValueOnce(head(404));
		expect(await revalidateAttachmentMetadata('ws', uuid, url)).toEqual({ status: 'missing' });
		expect(fetchMock).toHaveBeenCalledTimes(2);
	});

	it('leaves the authoritative missing result cached for later readers', async () => {
		const uuid = freshUuid();
		fetchMock.mockResolvedValueOnce(head(404));
		expect(await revalidateAttachmentMetadata('ws', uuid, url)).toEqual({ status: 'missing' });

		expect(await fetchAttachmentMetadata('ws', uuid, url)).toEqual({ status: 'missing' });
		expect(fetchMock).toHaveBeenCalledTimes(1);
	});

	it('does not cache a transient revalidation, so a retry probes again', async () => {
		const uuid = freshUuid();
		fetchMock.mockResolvedValueOnce(head(500));
		expect(await revalidateAttachmentMetadata('ws', uuid, url)).toEqual({ status: 'transient' });

		fetchMock.mockResolvedValueOnce(head(404));
		expect(await revalidateAttachmentMetadata('ws', uuid, url)).toEqual({ status: 'missing' });
		expect(fetchMock).toHaveBeenCalledTimes(2);
	});
});

describe('the no-store cache option reaches the dispatched request (PLAN-2392 3c-ii T6)', () => {
	// The always-revalidate-on-open probe must BYPASS the browser HTTP cache — the
	// GET/HEAD endpoint sets `Cache-Control: max-age=3600`, so a cached 200 would
	// hide a cross-tab / background delete. These assert on the actual fetch INIT
	// the mock received, not a local constant, so they fail if the option is dropped
	// anywhere between the caller and the wire.
	it('forwards cache:no-store onto the fetch init when asked', async () => {
		const uuid = freshUuid();
		fetchMock.mockResolvedValue(head(200, { 'content-type': 'image/png', 'content-length': '9' }));

		await fetchAttachmentMetadata('ws', uuid, url, { cache: 'no-store' });

		expect(fetchMock).toHaveBeenCalledWith(url(uuid), {
			method: 'HEAD',
			credentials: 'same-origin',
			cache: 'no-store',
		});
	});

	it('revalidate threads the cache option through invalidate-then-fetch', async () => {
		const uuid = freshUuid();
		fetchMock.mockResolvedValue(head(200, { 'content-type': 'image/png', 'content-length': '9' }));

		await revalidateAttachmentMetadata('ws', uuid, url, { cache: 'no-store' });

		expect(fetchMock).toHaveBeenCalledWith(url(uuid), {
			method: 'HEAD',
			credentials: 'same-origin',
			cache: 'no-store',
		});
	});

	it('revalidate+no-store reaches the network even when a cached ok exists, and carries no-store', async () => {
		// The property the surface RELIES on: a page-lifetime cached `ok` must not
		// suppress the existence probe, and the probe that runs must bypass the browser
		// HTTP cache. `revalidateAttachmentMetadata` invalidates the promise cache
		// first, so a SECOND fetch fires — and it carries `no-store`.
		const uuid = freshUuid();
		fetchMock.mockResolvedValue(head(200, { 'content-type': 'image/png', 'content-length': '10' }));

		// Prime the promise cache with a plain (cacheable) HEAD.
		await fetchAttachmentMetadata('ws', uuid, url);
		expect(fetchMock).toHaveBeenCalledTimes(1);

		// The forced open-probe: not served from the cached promise, and no-store.
		await revalidateAttachmentMetadata('ws', uuid, url, { cache: 'no-store' });
		expect(fetchMock).toHaveBeenCalledTimes(2);
		expect(fetchMock.mock.calls[1][1]).toEqual({
			method: 'HEAD',
			credentials: 'same-origin',
			cache: 'no-store',
		});
	});

	it('omits the cache field entirely when no option is passed — the plain HEAD is unchanged', async () => {
		const uuid = freshUuid();
		fetchMock.mockResolvedValue(head(200, { 'content-type': 'image/png', 'content-length': '9' }));

		await fetchAttachmentMetadata('ws', uuid, url);

		// No `cache` key at all — not `cache: undefined`. A default-mode HEAD, exactly
		// as before T6.
		const init = fetchMock.mock.calls[0][1] as RequestInit;
		expect('cache' in init).toBe(false);
	});
});

describe('mimeToFormat', () => {
	it('maps the recognized image MIMEs to the server-side format names', () => {
		expect(mimeToFormat('image/jpeg')).toBe('jpeg');
		expect(mimeToFormat('image/jpg')).toBe('jpeg');
		expect(mimeToFormat('image/heif')).toBe('heic');
		expect(mimeToFormat('IMAGE/PNG')).toBe('png');
	});

	it('returns null for non-images and unknown image subtypes', () => {
		expect(mimeToFormat('application/pdf')).toBeNull();
		expect(mimeToFormat('image/jxl')).toBeNull();
	});
});
