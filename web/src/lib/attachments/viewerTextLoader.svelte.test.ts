import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { createViewerTextLoader } from './viewerTextLoader.svelte';
import { TEXT_PREVIEW_MAX_BYTES } from './display';
import type { LightboxImage } from './events';

// IDEA-2712 / GitHub #1169 — the text loader. Unlike the image loader, whose
// request IS its `displaySrc`, this one issues a real `fetch`, so the acceptance
// is phrased in CALLS: a refused entry is "fetch was never called", a bounded
// body is "the phase is too-large and no text reached the consumer", a stale
// completion is "the state the user is looking at did not move".
//
// Every guard below is stated with the failure it exists to catch, because two
// of them (the metadata gate and the response gate) look redundant and are not:
// each has a case the other cannot see.

function entry(over: Partial<LightboxImage> = {}): LightboxImage {
	return {
		id: 'a1',
		alt: '',
		filename: 'notes.md',
		mime_type: 'text/markdown',
		size_bytes: 100,
		width: null,
		height: null,
		...over
	};
}

/** A Response whose body is NOT a stream — exercises the `response.text()` leg. */
function plainResponse(body: string, ok = true, status = 200): Response {
	return {
		ok,
		status,
		body: null,
		text: async () => body
	} as unknown as Response;
}

/**
 * A Response that streams `body` in chunks, exercising the `getReader` leg.
 *
 * `text()` THROWS (codex R3 #4). The first version of this fixture exposed the
 * whole body through `text()` as well, so an implementation that ignored the
 * stream and buffered everything passed the "bounds a body whose size the
 * metadata never declared" test — the exact behaviour those tests exist to
 * forbid. A fixture that satisfies both the right and the wrong implementation
 * discriminates nothing.
 *
 * The returned handle exposes `reads` and `cancelled` so a test can assert the
 * read STOPPED rather than merely that the phase ended up right: an
 * implementation that drains the whole stream and then measures reaches the
 * same `too-large`, and only the read count tells them apart.
 */
function streamedResponse(body: string, chunkSize = 8) {
	const bytes = new TextEncoder().encode(body);
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
		text: async () => {
			throw new Error('text() must not be used when a stream is available');
		}
	} as unknown as Response;
	return { response, state, chunkSize, totalBytes: bytes.length };
}

let fetchMock: ReturnType<typeof vi.fn>;

beforeEach(() => {
	fetchMock = vi.fn();
	vi.stubGlobal('fetch', fetchMock);
});

afterEach(() => {
	vi.unstubAllGlobals();
});

/** Let the loader's internal async chain settle. */
const settle = () => new Promise((r) => setTimeout(r, 0));

describe('createViewerTextLoader', () => {
	it('starts idle and asks for nothing', () => {
		const loader = createViewerTextLoader();
		expect(loader.phase).toBe('idle');
		expect(loader.text).toBe('');
		expect(fetchMock).not.toHaveBeenCalled();
	});

	it('loads a claimed entry and delivers the document', async () => {
		fetchMock.mockResolvedValue(plainResponse('# Title\n\nbody'));
		const loader = createViewerTextLoader();
		loader.load(entry(), 'ws');
		expect(loader.phase).toBe('loading');
		await settle();
		expect(loader.phase).toBe('ready');
		expect(loader.text).toBe('# Title\n\nbody');
		expect(fetchMock).toHaveBeenCalledTimes(1);
		// The ACL/variant path, not a raw storage read.
		expect(String(fetchMock.mock.calls[0][0])).toBe('/api/v1/workspaces/ws/attachments/a1');
	});

	it('reads a STREAMED body across chunk boundaries without corrupting multi-byte text', async () => {
		// The chunk size deliberately splits a multi-byte character, which a
		// non-streaming decoder would mangle — the reason `TextDecoder` is used
		// with `{ stream: true }` rather than decoding each chunk independently.
		const doc = '# café — naïve ☕ résumé';
		fetchMock.mockResolvedValue(streamedResponse(doc, 3).response);
		const loader = createViewerTextLoader();
		loader.load(entry({ size_bytes: null }), 'ws');
		await settle();
		expect(loader.phase).toBe('ready');
		expect(loader.text).toBe(doc);
	});

	// ── The request chokepoint ────────────────────────────────────────────────

	it('ISSUES NO REQUEST for a MIME this renderer does not claim', async () => {
		const loader = createViewerTextLoader();
		loader.load(entry({ mime_type: 'application/pdf', filename: 'x.pdf' }), 'ws');
		await settle();
		expect(loader.phase).toBe('idle');
		// The counterfactual: a renderer that merely SHOWED nothing would still
		// have fetched. The gate is at the request, so there is no call at all.
		expect(fetchMock).not.toHaveBeenCalled();
	});

	it('ISSUES NO REQUEST for the force-download bucket, which shares CategoryText with markdown', async () => {
		const loader = createViewerTextLoader();
		for (const mime of ['text/html', 'text/javascript', 'application/javascript']) {
			loader.load(entry({ mime_type: mime }), 'ws');
			await settle();
			expect(loader.phase).toBe('idle');
		}
		expect(fetchMock).not.toHaveBeenCalled();
	});

	// ── Gate 1: metadata ──────────────────────────────────────────────────────

	it('refuses an oversize entry from METADATA, without fetching', async () => {
		const loader = createViewerTextLoader();
		const size = TEXT_PREVIEW_MAX_BYTES + 1;
		loader.load(entry({ size_bytes: size }), 'ws');
		await settle();
		expect(loader.phase).toBe('too-large');
		expect(loader.oversizeBytes).toBe(size);
		// This gate's whole value is the transfer it saves.
		expect(fetchMock).not.toHaveBeenCalled();
	});

	it('admits an entry exactly AT the cap', async () => {
		fetchMock.mockResolvedValue(plainResponse('ok'));
		const loader = createViewerTextLoader();
		loader.load(entry({ size_bytes: TEXT_PREVIEW_MAX_BYTES }), 'ws');
		await settle();
		// The bound is "more than", not "as much as" — pinned so an off-by-one
		// tightening is a test failure rather than a silent behaviour change.
		expect(loader.phase).toBe('ready');
	});

	// ── Gate 2: response ──────────────────────────────────────────────────────

	it('bounds a body whose size the METADATA never declared', async () => {
		// THE CASE GATE 1 CANNOT SEE. `LightboxImage.size_bytes` is `number | null`
		// by declaration — an emitter knows only what its surface gave it — so a
		// null-size entry passes the metadata gate vacuously. Deleting the response
		// bound leaves exactly this entry unbounded, which is this test's mutant.
		const huge = 'x'.repeat(TEXT_PREVIEW_MAX_BYTES + 10);
		const stream = streamedResponse(huge, 4096);
		fetchMock.mockResolvedValue(stream.response);
		const loader = createViewerTextLoader();
		loader.load(entry({ size_bytes: null }), 'ws');
		await settle();
		expect(loader.phase).toBe('too-large');
		expect(loader.text).toBe('');
		// No figure: nothing declared one, so there is none to show.
		expect(loader.oversizeBytes).toBeNull();
		// IT STOPPED, rather than draining and then measuring. Both reach
		// `too-large`, so the phase alone cannot tell them apart — the read count
		// and the cancel can. Ceiling is the reads needed to cross the bound plus
		// the one that crosses it; draining would take ~257 at this chunk size.
		expect(stream.state.cancelled).toBe(true);
		expect(stream.state.reads).toBeLessThanOrEqual(
			Math.ceil(TEXT_PREVIEW_MAX_BYTES / stream.chunkSize) + 1
		);
	});

	it('bounds an UNDER-DECLARED body, and does NOT quote the false size', async () => {
		const huge = 'x'.repeat(TEXT_PREVIEW_MAX_BYTES + 10);
		fetchMock.mockResolvedValue(streamedResponse(huge, 4096).response);
		const loader = createViewerTextLoader();
		loader.load(entry({ size_bytes: 10 }), 'ws');
		await settle();
		expect(loader.phase).toBe('too-large');
		expect(loader.text).toBe('');
		// The declared 10 bytes is demonstrably wrong — the body outran a 1 MiB
		// bound — so echoing it would render "This file is 10 B — too large to
		// preview", which reads as a viewer bug rather than a file problem.
		expect(loader.oversizeBytes).toBeNull();
	});

	it('measures the bound in BYTES, not characters, on the non-streaming leg', async () => {
		// A multi-byte document just under the cap in characters but over it in
		// bytes must be refused. `String.length` would admit it.
		const chars = Math.floor(TEXT_PREVIEW_MAX_BYTES / 2) + 5; // 2 bytes each
		fetchMock.mockResolvedValue(plainResponse('é'.repeat(chars)));
		const loader = createViewerTextLoader();
		loader.load(entry({ size_bytes: null }), 'ws');
		await settle();
		expect(loader.phase).toBe('too-large');
	});

	// ── Staleness and cancellation ────────────────────────────────────────────

	it('drops a completion for an entry the user has already left', async () => {
		let resolveFirst: (r: Response) => void = () => {};
		fetchMock
			.mockImplementationOnce(() => new Promise<Response>((r) => (resolveFirst = r)))
			.mockResolvedValue(plainResponse('second doc'));

		const loader = createViewerTextLoader();
		loader.load(entry({ id: 'first' }), 'ws');
		loader.load(entry({ id: 'second' }), 'ws');
		await settle();
		expect(loader.text).toBe('second doc');

		// The first request now finishes, late.
		resolveFirst(plainResponse('FIRST DOC — must not appear'));
		await settle();
		// The end state alone would be ambiguous, so assert what the WRONG
		// behaviour would do: the first document's text reaching the consumer.
		expect(loader.text).toBe('second doc');
		expect(loader.phase).toBe('ready');
	});

	it('ABORTS the in-flight request when repointed', async () => {
		const signals: AbortSignal[] = [];
		fetchMock.mockImplementation((_url: string, init: RequestInit) => {
			signals.push(init.signal as AbortSignal);
			return new Promise<Response>(() => {});
		});
		const loader = createViewerTextLoader();
		loader.load(entry({ id: 'first' }), 'ws');
		expect(signals[0].aborted).toBe(false);
		loader.load(entry({ id: 'second' }), 'ws');
		// The registry-left-behind assertion: a loader that merely ignored the
		// stale result would leave this signal unaborted and the request running.
		expect(signals[0].aborted).toBe(true);
	});

	it('dispose() aborts and returns to idle', async () => {
		const signals: AbortSignal[] = [];
		fetchMock.mockImplementation((_url: string, init: RequestInit) => {
			signals.push(init.signal as AbortSignal);
			return new Promise<Response>(() => {});
		});
		const loader = createViewerTextLoader();
		loader.load(entry(), 'ws');
		loader.dispose();
		expect(signals[0].aborted).toBe(true);
		expect(loader.phase).toBe('idle');
		expect(loader.text).toBe('');
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
		const loader = createViewerTextLoader();
		loader.load(entry(), 'ws');
		loader.dispose();
		await settle();
		expect(loader.phase).toBe('idle');
	});

	// ── Errors and retry ──────────────────────────────────────────────────────

	it('reports a non-OK response as a retryable error', async () => {
		fetchMock.mockResolvedValue(plainResponse('nope', false, 500));
		const loader = createViewerTextLoader();
		loader.load(entry(), 'ws');
		await settle();
		expect(loader.phase).toBe('error');
		expect(loader.text).toBe('');
	});

	it('retry() re-requests from error and bumps the load token', async () => {
		fetchMock.mockResolvedValueOnce(plainResponse('nope', false, 500));
		const loader = createViewerTextLoader();
		loader.load(entry(), 'ws');
		await settle();
		const tokenAfterFail = loader.loadToken;

		fetchMock.mockResolvedValue(plainResponse('recovered'));
		loader.retry();
		await settle();
		expect(loader.phase).toBe('ready');
		expect(loader.text).toBe('recovered');
		expect(loader.loadToken).toBeGreaterThan(tokenAfterFail);
		expect(fetchMock).toHaveBeenCalledTimes(2);
	});

	it('retry() is INERT outside error — a too-large retry would reach the same answer', async () => {
		const loader = createViewerTextLoader();
		loader.load(entry({ size_bytes: TEXT_PREVIEW_MAX_BYTES + 1 }), 'ws');
		await settle();
		expect(loader.phase).toBe('too-large');
		loader.retry();
		await settle();
		expect(loader.phase).toBe('too-large');
		expect(fetchMock).not.toHaveBeenCalled();
	});

	it('retry() is INERT while LOADING — a second request would race the first', async () => {
		// The case the mutation matrix found uncovered: for `too-large` the guard
		// is redundant (`active` is already null), so removing `phase !== 'error'`
		// survived until this test existed. Here `active` IS set, so only the
		// phase check stands between one entry and two concurrent requests.
		fetchMock.mockImplementation(() => new Promise<Response>(() => {}));
		const loader = createViewerTextLoader();
		loader.load(entry(), 'ws');
		expect(loader.phase).toBe('loading');
		const tokenBefore = loader.loadToken;
		loader.retry();
		await settle();
		expect(fetchMock).toHaveBeenCalledTimes(1);
		expect(loader.loadToken).toBe(tokenBefore);
	});

	it('retry() is INERT from ready — there is nothing to recover', async () => {
		fetchMock.mockResolvedValue(plainResponse('doc'));
		const loader = createViewerTextLoader();
		loader.load(entry(), 'ws');
		await settle();
		expect(loader.phase).toBe('ready');
		loader.retry();
		await settle();
		expect(fetchMock).toHaveBeenCalledTimes(1);
	});

	it('load(undefined) releases the entry', async () => {
		fetchMock.mockResolvedValue(plainResponse('doc'));
		const loader = createViewerTextLoader();
		loader.load(entry(), 'ws');
		await settle();
		expect(loader.phase).toBe('ready');
		loader.load(undefined, 'ws');
		expect(loader.phase).toBe('idle');
		expect(loader.text).toBe('');
	});
});
