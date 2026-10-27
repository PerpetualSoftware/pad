/**
 * The ONE bounded byte reader behind the attachment surface's two fetch-based
 * loaders (TASK-4075 F1): {@link ./viewerBytesLoader} (PDF) and
 * {@link ./viewerTextLoader} (text/table, via {@link readBoundedText}).
 *
 * TWO CHECKS, with different reasons:
 *
 *  - PRECHECK: a declared `Content-Length` above `max` refuses BEFORE a single
 *    byte is read. This is what actually saves the transfer when the server is
 *    honest about the length. It only ever refuses: an absent, unparseable or
 *    under-stated header (compressed transfer, chunked encoding, a lying server)
 *    passes it and meets the streaming check.
 *  - STREAMING CHECK: counts the bytes accepted and cancels the reader once the
 *    total passes `max`.
 *
 * RESIDUAL OVERSHOOT, STATED RATHER THAN PAPERED OVER. The streaming check runs
 * after `reader.read()` has returned, so the chunk that crosses the cap is
 * already in memory when it is refused, and the contract sets no limit on chunk
 * size. Peak memory is therefore at most `max` plus one chunk, and one chunk is
 * bounded by what the network delivers, not by this module. The gate bounds what
 * is KEPT and RENDERED exactly; it does not bound peak memory exactly.
 *
 * THERE IS NO WHOLE-BUFFER FALLBACK. A response with no readable stream
 * (`body` null, or no `getReader`) is an ERROR here: the old fallback buffered
 * the entire response before measuring it, an unbounded path that shipped to
 * production for the sake of jsdom. Every browser this ships to streams; tests
 * stream a real `ReadableStream`.
 */

/** Thrown when the response cannot be streamed; the loaders land it in `error`. */
export class NotStreamableError extends Error {
	constructor() {
		super('response body is not a readable stream');
		this.name = 'NotStreamableError';
	}
}

/**
 * Read a response body into one buffer, refusing more than `max` bytes.
 * Returns `null` when the bound is exceeded (declared or observed).
 */
export async function readBoundedBytes(response: Response, max: number): Promise<Uint8Array | null> {
	const body = response.body;
	if (!body || typeof body.getReader !== 'function') throw new NotStreamableError();
	const declared = Number(response.headers?.get('content-length'));
	if (Number.isFinite(declared) && declared > max) {
		// Nothing has been read; release the connection without draining it.
		try {
			await body.cancel();
		} catch {
			// Best effort: the refusal does not depend on the release.
		}
		return null;
	}
	const reader = body.getReader();
	const chunks: Uint8Array[] = [];
	let seen = 0;
	for (;;) {
		const { done, value } = await reader.read();
		if (done) break;
		seen += value?.byteLength ?? 0;
		if (seen > max) {
			await reader.cancel();
			return null;
		}
		if (value) chunks.push(value);
	}
	const out = new Uint8Array(seen);
	let at = 0;
	for (const c of chunks) {
		out.set(c, at);
		at += c.byteLength;
	}
	return out;
}

/** {@link readBoundedBytes}, decoded as UTF-8. Returns `null` past `max`. */
export async function readBoundedText(response: Response, max: number): Promise<string | null> {
	const bytes = await readBoundedBytes(response, max);
	return bytes === null ? null : new TextDecoder().decode(bytes);
}
