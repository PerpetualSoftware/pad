/**
 * The attachment surface's BYTES loader (TASK-4075) — the PDF viewer's
 * counterpart to {@link ./viewerTextLoader}.
 *
 * The same contract as the text loader, deliberately: phases
 * idle/loading/ready/error/too-large, `oversizeBytes`, `loadToken`,
 * `load/retry/dispose`, the same two size gates, the same
 * {@link withRequestDeadline} linkage and the same staleness fence (token AND
 * id). What differs is the product: `bytes` (a `Uint8Array` that pdf.js will
 * parse) instead of decoded text. ONE LOADER PER BYTE SHAPE keeps the Lightbox's
 * "exactly one loader armed" rule simple, so the text loader's chokepoint is
 * NOT widened to admit PDF.
 *
 * It requests exactly {@link attachmentDownloadUrl} with no variant, never
 * navigates, and never creates a `blob:` URL: the bytes go to pdf.js as a
 * buffer, not as a URL. Fetching is safe for a type the server inlines for the
 * same reason as for one it does not: the bytes never become active same-origin
 * content, they are parsed by pdf.js and drawn to a canvas.
 *
 * TWO SIZE GATES, with independent reasons (see the text loader's header for the
 * long form; both are load-bearing and neither subsumes the other):
 *
 *  - METADATA gate: `size_bytes` above {@link PDF_PREVIEW_MAX_BYTES} makes no
 *    request at all. It passes vacuously for an entry that arrived without a size.
 *  - RESPONSE gate ({@link readBoundedBytes}, shared with the text loader): a
 *    declared Content-Length over the cap refuses before reading; otherwise the
 *    bytes accepted while streaming are counted and the reader is cancelled past
 *    the cap. A body that is not a stream is an `error`. The one-chunk overshoot
 *    that remains is disclosed in boundedBytes.ts.
 *
 * THE BYTES ARE THE LOADER'S, NOT PDF.JS'S. `getDocument({ data })` DETACHES
 * the buffer it is handed (measured: `byteLength` reads 0 afterwards), so the
 * consumer opens a COPY. The loader's buffer stays valid for a remount or a
 * second open of the same load.
 */
import { attachmentDownloadUrl } from '$lib/markdown/attachments';
import { withRequestDeadline } from '$lib/api/client';
import { canPreviewAsPdf, PDF_PREVIEW_MAX_BYTES } from '$lib/attachments/display';
import { readBoundedBytes } from './boundedBytes';
import type { LightboxImage } from './events';
import type { TextLoadPhase } from './viewerTextLoader.svelte';

export type BytesLoadPhase = TextLoadPhase;

export interface ViewerBytesLoader {
	/** The document once `phase === 'ready'`, else `null`. */
	readonly bytes: Uint8Array | null;
	readonly phase: BytesLoadPhase;
	/** The size that tripped `too-large`; set ONLY by the metadata gate (see the text loader). */
	readonly oversizeBytes: number | null;
	/** Changes on every fresh request, so a consumer can key off a real reload. */
	readonly loadToken: number;
	load(img: LightboxImage | undefined, wsSlug: string): void;
	/** Re-request the current entry after an `error`. Inert in any other phase. */
	retry(): void;
	/** Abort any in-flight request and return to `idle`. */
	dispose(): void;
}

interface ActiveLoad {
	img: LightboxImage;
	wsSlug: string;
	controller: AbortController;
}

export function createViewerBytesLoader(): ViewerBytesLoader {
	// `.raw`: a Uint8Array must never be proxied or deep-tracked.
	let bytes = $state.raw<Uint8Array | null>(null);
	let phase = $state<BytesLoadPhase>('idle');
	let oversizeBytes = $state<number | null>(null);
	let loadToken = $state(0);
	// Non-reactive: never read inside an $effect's tracked scope (CONVE-1688).
	let active: ActiveLoad | null = null;

	function toIdle(): void {
		active?.controller.abort();
		active = null;
		bytes = null;
		oversizeBytes = null;
		phase = 'idle';
	}

	function start(): void {
		const a = active;
		if (!a) return;
		// The gates, restated at the request chokepoint: neither load() nor retry()
		// can issue a request for a type this renderer does not claim.
		if (!canPreviewAsPdf(a.img.mime_type)) {
			toIdle();
			return;
		}
		const declared = a.img.size_bytes;
		if (typeof declared === 'number' && declared > PDF_PREVIEW_MAX_BYTES) {
			active = null;
			a.controller.abort();
			bytes = null;
			oversizeBytes = declared;
			phase = 'too-large';
			return;
		}
		const token = ++loadToken;
		const id = a.img.id;
		bytes = null;
		oversizeBytes = null;
		phase = 'loading';
		void (async () => {
			try {
				const body = await withRequestDeadline(
					async (signal) => {
						const res = await fetch(attachmentDownloadUrl(a.wsSlug, id), { signal });
						if (!res.ok) throw new Error(`HTTP ${res.status}`);
						return await readBoundedBytes(res, PDF_PREVIEW_MAX_BYTES);
					},
					{ idempotent: true, signal: a.controller.signal }
				);
				// THE STALENESS FENCE: the token catches a retry of the same entry, the
				// id catches navigation to a different one.
				if (token !== loadToken || active?.img.id !== id) return;
				if (body === null) {
					// The response outran the bound; the declared size is absent or
					// wrong, so no figure is quoted.
					oversizeBytes = null;
					bytes = null;
					phase = 'too-large';
					return;
				}
				bytes = body;
				phase = 'ready';
			} catch (err) {
				if ((err as { name?: string })?.name === 'AbortError') return;
				if (token !== loadToken || active?.img.id !== id) return;
				bytes = null;
				phase = 'error';
			}
		})();
	}

	function load(img: LightboxImage | undefined, wsSlug: string): void {
		toIdle();
		if (!img) return;
		active = { img, wsSlug, controller: new AbortController() };
		start();
	}

	function retry(): void {
		const a = active;
		if (!a || phase !== 'error') return;
		a.controller = new AbortController();
		start();
	}

	return {
		get bytes() {
			return bytes;
		},
		get phase() {
			return phase;
		},
		get oversizeBytes() {
			return oversizeBytes;
		},
		get loadToken() {
			return loadToken;
		},
		load,
		retry,
		dispose: toIdle
	};
}
