import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { createViewerBytesLoader } from './viewerBytesLoader.svelte';
import { PDF_PREVIEW_MAX_BYTES } from './display';
import type { LightboxImage } from './events';

// TASK-4075 U2 — the bytes loader behind the PDF viewer. Like the text loader it
// issues a real `fetch`, so the acceptance is phrased in CALLS and in what the
// consumer can observe. The two size gates look redundant and are not: each has
// a case the other cannot see.

function entry(over: Partial<LightboxImage> = {}): LightboxImage {
	return {
		id: 'a1',
		alt: '',
		filename: 'doc.pdf',
		mime_type: 'application/pdf',
		size_bytes: 100,
		width: null,
		height: null,
		...over
	};
}

const pdfBytes = (n: number, fill = 0x25) => new Uint8Array(n).fill(fill);

/** A real Response over a real ReadableStream (there is no non-stream fallback to exercise). */
function plainResponse(body: Uint8Array, ok = true, status = 200): Response {
	return new Response(body as BodyInit, { status: ok ? status : status === 200 ? 500 : status });
}

/** A Response whose body is absent: the shared reader refuses it as an error. */
function bodylessResponse(): Response {
	return { ok: true, status: 200, body: null, arrayBuffer: async () => new ArrayBuffer(4) } as unknown as Response;
}

/**
 * A streamed Response. arrayBuffer() THROWS, so an implementation that ignores
 * the stream and buffers everything cannot pass the response-gate test.
 */
function streamedResponse(bytes: Uint8Array, chunkSize = 4096) {
	let offset = 0;
	const state = { reads: 0, cancelled: false };
	const reader = {
		read: async () => {
			state.reads++;
			if (offset >= bytes.length) return { done: true, value: undefined };
			const value = bytes.slice(offset, offset + chunkSize);
			offset += chunkSize;
			return { done: false, value };
		},
		cancel: async () => {
			state.cancelled = true;
		}
	};
	const response = {
		ok: true,
		status: 200,
		body: { getReader: () => reader },
		arrayBuffer: async () => {
			throw new Error('arrayBuffer() must not be used when a stream is available');
		}
	} as unknown as Response;
	return { response, state };
}

let fetchMock: ReturnType<typeof vi.fn>;

beforeEach(() => {
	fetchMock = vi.fn();
	vi.stubGlobal('fetch', fetchMock);
});
afterEach(() => {
	vi.unstubAllGlobals();
});

const settle = () => new Promise((r) => setTimeout(r, 0));

describe('createViewerBytesLoader', () => {
	it('starts idle and asks for nothing', () => {
		const loader = createViewerBytesLoader();
		expect(loader.phase).toBe('idle');
		expect(loader.bytes).toBeNull();
		expect(fetchMock).not.toHaveBeenCalled();
	});

	it('loads a PDF and delivers the bytes from exactly the download URL, no variant', async () => {
		const doc = pdfBytes(50);
		fetchMock.mockResolvedValue(plainResponse(doc));
		const loader = createViewerBytesLoader();
		loader.load(entry(), 'ws');
		expect(loader.phase).toBe('loading');
		await settle();
		expect(loader.phase).toBe('ready');
		expect(loader.bytes).toEqual(doc);
		expect(fetchMock).toHaveBeenCalledTimes(1);
		expect(String(fetchMock.mock.calls[0][0])).toBe('/api/v1/workspaces/ws/attachments/a1');
	});

	it('reassembles a STREAMED body across chunk boundaries byte-for-byte', async () => {
		const doc = Uint8Array.from({ length: 10_000 }, (_, i) => i % 251);
		fetchMock.mockResolvedValue(streamedResponse(doc, 777).response);
		const loader = createViewerBytesLoader();
		loader.load(entry({ size_bytes: null }), 'ws');
		await settle();
		expect(loader.phase).toBe('ready');
		expect(loader.bytes).toEqual(doc);
	});

	it.each(['text/plain', 'image/png', 'application/x-pdf', 'application/octet-stream', null])(
		'requests nothing for a type this loader does not claim (%s)',
		async (mime) => {
			const loader = createViewerBytesLoader();
			loader.load(entry({ mime_type: mime }), 'ws');
			await settle();
			expect(loader.phase).toBe('idle');
			expect(fetchMock).not.toHaveBeenCalled();
		}
	);

	// ── The two gates ─────────────────────────────────────────────────────────

	it('METADATA gate: refuses an oversize entry without a request and reports the size', async () => {
		const loader = createViewerBytesLoader();
		loader.load(entry({ size_bytes: PDF_PREVIEW_MAX_BYTES + 1 }), 'ws');
		await settle();
		expect(fetchMock).not.toHaveBeenCalled();
		expect(loader.phase).toBe('too-large');
		expect(loader.oversizeBytes).toBe(PDF_PREVIEW_MAX_BYTES + 1);
		expect(loader.bytes).toBeNull();
	});

	it('METADATA gate: a file exactly at the cap is fetched', async () => {
		fetchMock.mockResolvedValue(plainResponse(pdfBytes(10)));
		const loader = createViewerBytesLoader();
		loader.load(entry({ size_bytes: PDF_PREVIEW_MAX_BYTES }), 'ws');
		await settle();
		expect(fetchMock).toHaveBeenCalledTimes(1);
		expect(loader.phase).toBe('ready');
	});

	it('RESPONSE gate: bounds an UNDECLARED body, cancels the reader and does not quote a size', async () => {
		const huge = new Uint8Array(PDF_PREVIEW_MAX_BYTES + 3 * (1 << 20));
		const stream = streamedResponse(huge, 1 << 20);
		fetchMock.mockResolvedValue(stream.response);
		const loader = createViewerBytesLoader();
		loader.load(entry({ size_bytes: null }), 'ws');
		await settle();
		expect(loader.phase).toBe('too-large');
		expect(loader.oversizeBytes).toBeNull();
		expect(loader.bytes).toBeNull();
		// The read STOPPED: a drain-then-measure implementation reaches the same
		// phase and only the read count and the cancel tell them apart.
		expect(stream.state.cancelled).toBe(true);
		expect(stream.state.reads).toBeLessThanOrEqual(PDF_PREVIEW_MAX_BYTES / (1 << 20) + 2);
	});

	it('RESPONSE gate: an UNDER-DECLARED size does not slip past, and the false size is not quoted', async () => {
		const stream = streamedResponse(new Uint8Array(PDF_PREVIEW_MAX_BYTES + 5), 1 << 20);
		fetchMock.mockResolvedValue(stream.response);
		const loader = createViewerBytesLoader();
		loader.load(entry({ size_bytes: 10 }), 'ws');
		await settle();
		expect(loader.phase).toBe('too-large');
		expect(loader.oversizeBytes).toBeNull();
	});

	it('RESPONSE gate: a real stream past the cap is too-large', async () => {
		fetchMock.mockResolvedValue(plainResponse(new Uint8Array(PDF_PREVIEW_MAX_BYTES + 1)));
		const loader = createViewerBytesLoader();
		loader.load(entry({ size_bytes: null }), 'ws');
		await settle();
		expect(loader.phase).toBe('too-large');
		expect(loader.bytes).toBeNull();
	});

	it('RESPONSE gate: a declared Content-Length over the cap refuses BEFORE reading', async () => {
		const stream = streamedResponse(pdfBytes(100), 10);
		(stream.response as unknown as { headers: Headers }).headers = new Headers({
			'content-length': String(PDF_PREVIEW_MAX_BYTES + 1)
		});
		fetchMock.mockResolvedValue(stream.response);
		const loader = createViewerBytesLoader();
		loader.load(entry({ size_bytes: null }), 'ws');
		await settle();
		expect(loader.phase).toBe('too-large');
		expect(loader.bytes).toBeNull();
		expect(stream.state.reads).toBe(0);
	});

	it('RESPONSE gate: a missing body is the error phase, not a buffered read', async () => {
		fetchMock.mockResolvedValue(bodylessResponse());
		const loader = createViewerBytesLoader();
		loader.load(entry({ size_bytes: null }), 'ws');
		await settle();
		expect(loader.phase).toBe('error');
		expect(loader.bytes).toBeNull();
	});

	// ── Staleness and cancellation ────────────────────────────────────────────

	it('drops a completion for an entry the user has already left (id half of the fence)', async () => {
		let resolveFirst: (r: Response) => void = () => {};
		fetchMock
			.mockImplementationOnce(() => new Promise<Response>((r) => (resolveFirst = r)))
			.mockResolvedValue(plainResponse(pdfBytes(5, 0x32)));
		const loader = createViewerBytesLoader();
		loader.load(entry({ id: 'first' }), 'ws');
		loader.load(entry({ id: 'second' }), 'ws');
		await settle();
		expect(loader.bytes).toEqual(pdfBytes(5, 0x32));
		resolveFirst(plainResponse(pdfBytes(5, 0x31)));
		await settle();
		expect(loader.bytes).toEqual(pdfBytes(5, 0x32));
		expect(loader.phase).toBe('ready');
	});

	it('drops a late completion of the SAME entry after a retry (token half of the fence)', async () => {
		let resolveFirst: (r: Response) => void = () => {};
		fetchMock.mockImplementationOnce(() => new Promise<Response>((r) => (resolveFirst = r)));
		const loader = createViewerBytesLoader();
		loader.load(entry(), 'ws');
		// Force an error state so retry() is allowed, without finishing request 1.
		// Request 1 is made stale by a second request for the same id.
		fetchMock.mockResolvedValueOnce(plainResponse(pdfBytes(3, 0x32)));
		// Re-load of the same id issues a new request and a new token.
		loader.load(entry(), 'ws');
		await settle();
		expect(loader.bytes).toEqual(pdfBytes(3, 0x32));
		resolveFirst(plainResponse(pdfBytes(3, 0x31)));
		await settle();
		expect(loader.bytes).toEqual(pdfBytes(3, 0x32));
	});

	it('drops a completion that arrives after dispose() (the id half of the fence: no new request, so the token is unchanged)', async () => {
		// dispose() starts no request, so `loadToken` still equals the late request's token;
		// only `active` being gone tells the completion it is stale. A fetch that ignores the
		// abort (a mock, or a response already in flight) is exactly the late arrival.
		let resolveLate: (r: Response) => void = () => {};
		fetchMock.mockImplementationOnce(() => new Promise<Response>((r) => (resolveLate = r)));
		const loader = createViewerBytesLoader();
		loader.load(entry(), 'ws');
		loader.dispose();
		resolveLate(plainResponse(pdfBytes(9)));
		await settle();
		expect(loader.phase).toBe('idle');
		expect(loader.bytes).toBeNull();
	});

	it('an AbortError that reaches the loader while it is still the active load is SILENT, not an error', async () => {
		// The browser (or an extension) can abort a request the loader did not: there is no
		// dispose and no repoint, so the staleness fence passes. The spec is that an abort
		// never reads as a failure; the loader keeps showing its loading state.
		fetchMock.mockImplementationOnce(() => {
			const e = new Error('aborted');
			e.name = 'AbortError';
			return Promise.reject(e);
		});
		const loader = createViewerBytesLoader();
		loader.load(entry(), 'ws');
		await settle();
		expect(loader.phase).toBe('loading');
	});

	it('ABORTS the in-flight request when repointed, and on dispose()', async () => {
		const signals: AbortSignal[] = [];
		fetchMock.mockImplementation((_url: string, init: RequestInit) => {
			signals.push(init.signal as AbortSignal);
			return new Promise<Response>(() => {});
		});
		const loader = createViewerBytesLoader();
		loader.load(entry({ id: 'first' }), 'ws');
		loader.load(entry({ id: 'second' }), 'ws');
		expect(signals[0].aborted).toBe(true);
		loader.dispose();
		expect(signals[1].aborted).toBe(true);
		expect(loader.phase).toBe('idle');
		expect(loader.bytes).toBeNull();
	});

	it('does not report an abort as an error', async () => {
		fetchMock.mockImplementation(
			(_url: string, init: RequestInit) =>
				new Promise<Response>((_res, rej) => {
					(init.signal as AbortSignal).addEventListener('abort', () => {
						const e = new Error('aborted');
						e.name = 'AbortError';
						rej(e);
					});
				})
		);
		const loader = createViewerBytesLoader();
		loader.load(entry(), 'ws');
		loader.dispose();
		await settle();
		expect(loader.phase).toBe('idle');
	});

	// ── Errors and retry ──────────────────────────────────────────────────────

	it('reports a non-OK response as a retryable error, and retry() recovers with a new token', async () => {
		fetchMock.mockResolvedValueOnce(plainResponse(pdfBytes(1), false, 500));
		const loader = createViewerBytesLoader();
		loader.load(entry(), 'ws');
		await settle();
		expect(loader.phase).toBe('error');
		expect(loader.bytes).toBeNull();
		const tokenAfterFail = loader.loadToken;
		fetchMock.mockResolvedValue(plainResponse(pdfBytes(4)));
		loader.retry();
		await settle();
		expect(loader.phase).toBe('ready');
		expect(loader.loadToken).toBeGreaterThan(tokenAfterFail);
	});

	it('retry() is INERT from too-large, loading and ready', async () => {
		const loader = createViewerBytesLoader();
		loader.load(entry({ size_bytes: PDF_PREVIEW_MAX_BYTES + 1 }), 'ws');
		loader.retry();
		expect(loader.phase).toBe('too-large');
		expect(fetchMock).not.toHaveBeenCalled();

		fetchMock.mockImplementation(() => new Promise<Response>(() => {}));
		loader.load(entry(), 'ws');
		const t = loader.loadToken;
		loader.retry();
		expect(fetchMock).toHaveBeenCalledTimes(1);
		expect(loader.loadToken).toBe(t);

		fetchMock.mockReset();
		fetchMock.mockResolvedValue(plainResponse(pdfBytes(2)));
		loader.load(entry({ id: 'b' }), 'ws');
		await settle();
		loader.retry();
		expect(fetchMock).toHaveBeenCalledTimes(1);
	});

	it('load(undefined) releases the entry and its bytes', async () => {
		fetchMock.mockResolvedValue(plainResponse(pdfBytes(8)));
		const loader = createViewerBytesLoader();
		loader.load(entry(), 'ws');
		await settle();
		loader.load(undefined, 'ws');
		expect(loader.phase).toBe('idle');
		expect(loader.bytes).toBeNull();
	});
});
