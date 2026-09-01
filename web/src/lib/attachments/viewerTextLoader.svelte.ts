/**
 * The attachment surface's TEXT loader (IDEA-2712 / GitHub #1169) — the
 * markdown / plain-text counterpart to {@link ./viewerImageLoader}.
 *
 * MODEL, and how it differs from the image loader. The image loader hands a URL
 * to an `<img>` and lets the BROWSER fetch, decode, cache and cancel. Text has
 * no such element: we `fetch()` the bytes ourselves and render them. Three
 * consequences the image loader does not have to think about, all handled here:
 *
 *  1. **We own cancellation.** No `src` reassignment drops the previous request,
 *     so every load carries an `AbortController` and repointing aborts it. A
 *     surface arrowed through twenty attachments must leave no request running.
 *  2. **We own staleness.** A late `await` can resolve after the user has moved
 *     on (the no-`{#key}` switch-safety class the image loader's `decoded()`
 *     fence exists for). Every completion here is checked against the request
 *     token that issued it AND the currently-active id; a stale one is dropped
 *     without touching state.
 *  3. **We own the size bound.** The browser will happily stream a 20 MB "log
 *     file" renamed `.md` into memory. See the two gates below — with one honest
 *     limit: the non-streaming fallback in {@link readBounded} measures AFTER
 *     buffering, so on an environment with no `ReadableStream` the bytes are
 *     held before they are refused. It bounds what is RENDERED everywhere; it
 *     bounds what is HELD only on the streaming path, which is every real
 *     browser. jsdom is the fallback's practical audience.
 *
 * WHY FETCHING IS SAFE FOR TYPES THE SERVER WILL NOT INLINE. `text/markdown` is
 * served `Content-Disposition: attachment` (`internal/attachments/mime.go`'s
 * `inlineSafe` admits only PDF and `text/plain` among non-media types). That
 * disposition governs what the BROWSER does with a top-level navigation; it does
 * not impede `fetch()`, and it is not what makes this safe. What makes it safe is
 * that the bytes never become active same-origin content: they are rendered
 * through the shared markdown pipeline and sanitized, exactly like item content.
 * PLAN-2393 DR-6's rule — never inline a force-download type — is honoured by
 * the ALLOWLIST (`canPreviewAsText` admits neither HTML nor JavaScript), not by
 * the disposition header.
 *
 * TWO SIZE GATES, WITH INDEPENDENT REASONS. Both are load-bearing and neither
 * subsumes the other:
 *
 *  - The METADATA gate (before any request) reads `size_bytes` and refuses to
 *    fetch at all above {@link TEXT_PREVIEW_MAX_BYTES}. This is the one that
 *    saves the transfer.
 *  - The RESPONSE gate (while reading) bounds the bytes actually accepted.
 *    `LightboxImage.size_bytes` is `number | null` BY DECLARATION — an emitter
 *    knows only what its own surface gave it — so the metadata gate passes
 *    VACUOUSLY for any entry that arrived without a size. Without the response
 *    gate those entries are unbounded.
 *
 * Deleting either one leaves a real hole, which is the test each must fail.
 */
import { attachmentDownloadUrl } from '$lib/markdown/attachments';
import { canPreviewAsText, TEXT_PREVIEW_MAX_BYTES } from '$lib/attachments/display';
import type { LightboxImage } from './events';

export type TextLoadPhase =
	/** Nothing to load: no entry, or one this renderer does not claim. */
	| 'idle'
	/** A request is in flight. */
	| 'loading'
	/** `text` holds the document. */
	| 'ready'
	/** The fetch failed. Retryable. */
	| 'error'
	/**
	 * The document is past {@link TEXT_PREVIEW_MAX_BYTES}. A TERMINAL state, not
	 * an error: nothing went wrong and a retry would reach the same answer, so
	 * the surface offers its existing download affordance instead of a retry.
	 */
	| 'too-large';

export interface ViewerTextLoader {
	/** The document text once `phase === 'ready'`, else `''`. */
	readonly text: string;
	readonly phase: TextLoadPhase;
	/**
	 * The size that tripped `too-large`, for the surface's message — set ONLY when
	 * the METADATA gate refused (a trustworthy declared size). `null` otherwise,
	 * including when the RESPONSE outran the bound: there the declared size is
	 * absent or wrong by definition, so there is no honest figure to show.
	 */
	readonly oversizeBytes: number | null;
	/** Changes on every fresh request, so a consumer can key off a real reload. */
	readonly loadToken: number;
	/** Point the loader at an entry (or `undefined` to release). */
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

/**
 * Read a response body, refusing more than `max` bytes.
 *
 * Streams so an oversize body is abandoned mid-flight rather than buffered and
 * then measured — the point is not to hold 20 MB in memory long enough to
 * decide we did not want it.
 *
 * THE FALLBACK IS WEAKER, AND KNOWINGLY SO. Where the body is not a readable
 * stream (jsdom, and any environment without `getReader`) this reads the whole
 * response and then measures it: the RENDER bound still holds, the MEMORY bound
 * does not. Stated rather than papered over, because the module header claims
 * to own the size bound and that claim is only fully true on the streaming path
 * (codex R1 #2). Every browser this ships to has streams; the fallback's real
 * audience is the test environment.
 *
 * Returns `null` when the bound is exceeded.
 */
async function readBounded(response: Response, max: number): Promise<string | null> {
	const body = response.body;
	if (!body || typeof body.getReader !== 'function') {
		const whole = await response.text();
		// Byte length, not string length: the bound is a byte bound, and a
		// multi-byte document would otherwise be measured short.
		return new TextEncoder().encode(whole).length > max ? null : whole;
	}
	const reader = body.getReader();
	const decoder = new TextDecoder();
	let seen = 0;
	let out = '';
	for (;;) {
		const { done, value } = await reader.read();
		if (done) break;
		seen += value?.byteLength ?? 0;
		if (seen > max) {
			await reader.cancel();
			return null;
		}
		out += decoder.decode(value, { stream: true });
	}
	out += decoder.decode();
	return out;
}

export function createViewerTextLoader(): ViewerTextLoader {
	let text = $state('');
	let phase = $state<TextLoadPhase>('idle');
	let oversizeBytes = $state<number | null>(null);
	let loadToken = $state(0);
	// Non-reactive: never read inside an $effect's tracked scope, so writing it
	// cannot self-invalidate a flush (CONVE-1688).
	let active: ActiveLoad | null = null;

	/** One cleanup, so every "give up on this entry" site leaves the same state. */
	function toIdle(): void {
		active?.controller.abort();
		active = null;
		text = '';
		oversizeBytes = null;
		phase = 'idle';
	}

	function start(): void {
		const a = active;
		if (!a) return;
		// THE GATES, RESTATED AT THE REQUEST CHOKEPOINT — not only at the renderer.
		// `load()` and `retry()` both funnel through here, so neither can issue a
		// request for a type this renderer does not claim. The image loader states
		// the reason and it holds identically: the renderer showing nothing is not
		// the same as the loader asking for nothing.
		if (!canPreviewAsText(a.img.mime_type)) {
			toIdle();
			return;
		}
		const declared = a.img.size_bytes;
		if (typeof declared === 'number' && declared > TEXT_PREVIEW_MAX_BYTES) {
			// Terminal, and no request is made at all.
			active = null;
			a.controller.abort();
			text = '';
			oversizeBytes = declared;
			phase = 'too-large';
			return;
		}
		const token = ++loadToken;
		const id = a.img.id;
		text = '';
		oversizeBytes = null;
		phase = 'loading';
		void (async () => {
			try {
				const res = await fetch(attachmentDownloadUrl(a.wsSlug, id), {
					signal: a.controller.signal
				});
				if (!res.ok) throw new Error(`HTTP ${res.status}`);
				const body = await readBounded(res, TEXT_PREVIEW_MAX_BYTES);
				// THE STALENESS FENCE. Both halves are needed: the token catches a
				// retry of the SAME entry (id unchanged), the id catches navigation
				// to a different one (token could coincide only by accident, but the
				// id is the thing the user is actually looking at).
				if (token !== loadToken || active?.img.id !== id) return;
				if (body === null) {
					// The response outran the bound. `oversizeBytes` stays NULL here
					// (codex R2 #3): the declared size is either absent or — in the
					// under-declaring case this branch exists to catch — WRONG, and
					// echoing it produces "This file is 10 B — too large to preview",
					// a sentence that reads as a bug in the viewer rather than a
					// problem with the file. The true size is unknown by construction
					// (we stopped reading), so the surface says "too large" without a
					// figure. The metadata branch above, which HAS a trustworthy
					// number, is the only one that reports one.
					oversizeBytes = null;
					text = '';
					phase = 'too-large';
					return;
				}
				text = body;
				phase = 'ready';
			} catch (err) {
				if ((err as { name?: string })?.name === 'AbortError') return;
				if (token !== loadToken || active?.img.id !== id) return;
				text = '';
				phase = 'error';
			}
		})();
	}

	function load(img: LightboxImage | undefined, wsSlug: string): void {
		// Repoint: abort whatever was running. Unlike the image loader there is no
		// `src` reassignment doing this for us.
		toIdle();
		if (!img) return;
		active = { img, wsSlug, controller: new AbortController() };
		start();
	}

	function retry(): void {
		const a = active;
		// Only from `error`. A `too-large` retry would reach the same answer, and
		// retrying from `loading` would race two requests for one entry.
		if (!a || phase !== 'error') return;
		a.controller = new AbortController();
		start();
	}

	return {
		get text() {
			return text;
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
