// BUG-3475: the workspace-bundle upload, over XMLHttpRequest.
//
// It used to be a bare `fetch`, which has no upload progress and no way to
// notice a stall: on a path that dropped packets (QUIC to Cloudflare) the
// promise never settled and the import dialog sat on "Importing..." forever.
// XHR reports bytes as they leave (`upload.onprogress`), so a stall is
// "no progress for STALL_MS", not a wall clock: large bundles are
// legitimate and slow links are not stalls.
//
// Two bounds, because a browser counts bytes as sent once its network stack
// has them, so a dead path can also show up only AFTER 100%:
//   STALL_MS      no upload progress for this long while uploading;
//   FINISHING_MS  no response this long after the last byte.
// Both are pinned against the server's per-Read idle window
// (internal/server/import_read_deadline.go) by importUpload.thresholds.test.ts:
// stall <= idle, so the client never waits longer than the server would;
// finishing > idle, so a server still finishing is given the time it has.

export const IMPORT_STALL_MS = 60_000;
export const IMPORT_FINISHING_MS = 120_000;

/** Why the client gave up. The OUTCOME on the server is not known from this. */
export type ImportTransportFailure =
	| 'stalled' // no upload progress for IMPORT_STALL_MS
	| 'no_response' // uploaded, then no response for IMPORT_FINISHING_MS
	| 'network' // the browser reported a network error
	| 'aborted'; // the caller cancelled

export class ImportTransportError extends Error {
	constructor(readonly kind: ImportTransportFailure) {
		super(
			kind === 'stalled'
				? 'The upload stopped making progress'
				: kind === 'no_response'
					? 'The server did not answer after the upload finished'
					: kind === 'network'
						? 'The connection failed during the import'
						: 'The import was cancelled'
		);
		this.name = 'ImportTransportError';
	}
}

export interface ImportUploadResponse {
	status: number;
	body: string;
	header(name: string): string | null;
}

export interface ImportUploadOptions {
	url: string;
	headers: Record<string, string>;
	body: Blob;
	/** Bytes handed to the network so far, of total. */
	onProgress?: (sent: number, total: number) => void;
	/** The last byte left; the server is finishing the import. */
	onUploaded?: () => void;
	signal?: AbortSignal;
	/** Seams for tests. */
	xhr?: () => XMLHttpRequest;
	setTimer?: (fn: () => void, ms: number) => unknown;
	clearTimer?: (id: unknown) => void;
}

/**
 * POST `body` with progress and both bounds. Resolves with the response, of
 * any status; rejects ONLY with ImportTransportError, when no response was
 * received at all.
 */
export function uploadImportBundle(opts: ImportUploadOptions): Promise<ImportUploadResponse> {
	const setTimer = opts.setTimer ?? ((fn: () => void, ms: number) => setTimeout(fn, ms));
	const clearTimer = opts.clearTimer ?? ((id: unknown) => clearTimeout(id as ReturnType<typeof setTimeout>));
	const xhr = (opts.xhr ?? (() => new XMLHttpRequest()))();

	return new Promise((resolve, reject) => {
		let timer: unknown = null;
		let settled = false;
		const stop = () => {
			if (timer !== null) clearTimer(timer);
			timer = null;
			opts.signal?.removeEventListener('abort', onAbort);
		};
		const fail = (kind: ImportTransportFailure) => {
			if (settled) return;
			settled = true;
			stop();
			// Abort AFTER marking settled: abort() fires onabort synchronously
			// in some engines, which must not report a second failure.
			try {
				xhr.abort();
			} catch {
				/* already done */
			}
			reject(new ImportTransportError(kind));
		};
		const arm = (ms: number, kind: ImportTransportFailure) => {
			if (timer !== null) clearTimer(timer);
			timer = setTimer(() => fail(kind), ms);
		};
		const onAbort = () => fail('aborted');
		// A caller's callback must not wedge the upload: one that throws is
		// reported and ignored, and the transfer and its timers carry on
		// (codex r1).
		const notify = (fn: () => void) => {
			try {
				fn();
			} catch (err) {
				console.error('import upload callback threw', err);
			}
		};

		if (opts.signal?.aborted) {
			settled = true;
			reject(new ImportTransportError('aborted'));
			return;
		}
		opts.signal?.addEventListener('abort', onAbort);

		xhr.open('POST', opts.url);
		xhr.withCredentials = true;
		for (const [k, v] of Object.entries(opts.headers)) xhr.setRequestHeader(k, v);

		xhr.upload.onprogress = (e: ProgressEvent) => {
			if (settled) return;
			arm(IMPORT_STALL_MS, 'stalled');
			notify(() => opts.onProgress?.(e.loaded, e.lengthComputable ? e.total : opts.body.size));
		};
		xhr.upload.onload = () => {
			if (settled) return;
			arm(IMPORT_FINISHING_MS, 'no_response');
			notify(() => opts.onUploaded?.());
		};
		xhr.onload = () => {
			if (settled) return;
			settled = true;
			stop();
			resolve({
				status: xhr.status,
				body: xhr.responseText,
				header: (name: string) => xhr.getResponseHeader(name)
			});
		};
		xhr.onerror = () => fail('network');
		xhr.ontimeout = () => fail('network');
		xhr.onabort = () => fail('aborted');

		// Armed before the first byte: a path dead from the start reports no
		// progress at all.
		arm(IMPORT_STALL_MS, 'stalled');
		notify(() => opts.onProgress?.(0, opts.body.size));
		xhr.send(opts.body);
	});
}
