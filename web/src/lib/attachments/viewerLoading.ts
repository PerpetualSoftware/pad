/**
 * DR-5b image-loading policy for the attachment viewer (PLAN-2392 / TASK-2459).
 *
 * The memory-safety half of DR-5b: decide, from an image's PIXEL dimensions and
 * the platform, what the viewer requests first and whether it upgrades to the
 * original in the background — so a desktop promised a bounded first paint never
 * silently decodes a 50 MP original twice, and a phone never auto-pulls one at
 * all.
 *
 * PIXELS, NEVER BYTES. `size_bytes` is COMPRESSED size; a 3 MB JPEG can decode
 * to 50 MP. The only honest ceiling for "how much RAM will this bitmap cost" is
 * `width x height x 4` (RGBA), so classification reads dimensions and nothing
 * else. When a dimension is missing the class is {@link SizeClass.unknown} — a
 * genuine third value, NOT an alias for `large`: only the mobile
 * no-auto-request decision treats the two alike; the desktop paths differ
 * (unknown asks for the original directly, large asks for the thumbnail).
 *
 * This is a MEMORY POLICY, not a security boundary — the server authorizes the
 * parent before it resolves any variant (`handlers_storage.go`). It is also
 * FORMAT-BLIND on purpose: the load-bearing piece, {@link servedOriginal}, reads
 * the decoded bitmap's own long edge rather than mirroring the server's decoder
 * set, which would be a silently-rotting copy (the mirror DR-3a's shared fixture
 * exists to avoid).
 */

/**
 * The `thumb-md` variant's long-edge bound, in CSS px — it mirrors the server's
 * (`handlers_attachments.go`: a 1024 px long-edge derivation). A decoded bitmap
 * whose long edge EXCEEDS this cannot be that thumbnail, which is exactly what
 * {@link servedOriginal} keys off.
 */
export const THUMB_LONG_EDGE = 1024;

/**
 * The pixel ceiling for auto-fetching an ORIGINAL without asking: ~8 MP, about
 * 32 MiB decoded RGBA. At or below it a dimensioned image is `small`; above it
 * `large`. This is a CLIENT policy threshold, distinct from the server's 64 MP
 * upload ceiling (`MaxPixelsDefault`).
 */
export const AUTO_LOAD_MAX_PIXELS = 8_000_000;

/**
 * The size class of an image, from its pixels alone.
 *
 *  - `small`   — dimensioned, at or under {@link AUTO_LOAD_MAX_PIXELS}.
 *  - `large`   — dimensioned, over it.
 *  - `unknown` — a dimension is missing / non-finite / non-positive. A THIRD
 *    value, never folded into `large`.
 */
export type SizeClass = 'small' | 'large' | 'unknown';

export type Platform = 'desktop' | 'mobile';

function usableDimension(value: number | null | undefined): value is number {
	return typeof value === 'number' && Number.isFinite(value) && value > 0;
}

/**
 * Classify on `width x height` and NOTHING else (never `size_bytes`).
 *
 * `classify(null, 900) === 'unknown'` — one missing dimension is enough, because
 * a policy that can't see one axis can't bound the pixel count. A dimensioned
 * image is `large` strictly ABOVE the ceiling, so an exactly-ceiling image is
 * still `small`.
 */
export function classify(
	width: number | null | undefined,
	height: number | null | undefined
): SizeClass {
	if (!usableDimension(width) || !usableDimension(height)) return 'unknown';
	return width * height > AUTO_LOAD_MAX_PIXELS ? 'large' : 'small';
}

/**
 * True when the image's OWN pixels already fit inside the thumbnail bound, so
 * the server would skip derivation and the download IS the original. Only
 * meaningful for a dimensioned (`small`) image; `unknown` can't answer it.
 */
function withinThumbBound(width: number, height: number): boolean {
	return Math.max(width, height) <= THUMB_LONG_EDGE;
}

/** Which variant the viewer requests for FIRST paint, and whether it upgrades. */
export type FirstRequest =
	/** Request the original directly; there is no separate upgrade. */
	| { variant: 'original' }
	/**
	 * Request the bounded thumbnail. `upgrade` is whether to background-fetch the
	 * original once it paints (desktop yes; mobile no — the original arrives on
	 * zoom past fit, TASK-2460). Suppressed anyway if {@link servedOriginal}.
	 */
	| { variant: 'thumb-md'; upgrade: boolean }
	/** Request NOTHING now — the mobile tap affordance loads it (TASK-2460). */
	| { variant: null };

/**
 * The DR-5b decision table, as one pure function.
 *
 * | dims                       | desktop                    | mobile                 |
 * | -------------------------- | -------------------------- | ---------------------- |
 * | small, long edge <= 1024   | original directly          | original directly      |
 * | small, long edge > 1024    | thumb-md, upgrade           | thumb-md, no upgrade    |
 * | large (dims known)         | thumb-md, upgrade           | nothing (tap)          |
 * | unknown dims               | original directly          | nothing (tap)          |
 *
 * The `unknown`-desktop cell asks for the ORIGINAL, not a thumb: an unknown
 * image whose original is <= 1024 px would defeat {@link servedOriginal} (the
 * fallback serves that bounded original and the detector, seeing <= 1024,
 * wrongly concludes it got a real thumbnail and fetches again). Requesting the
 * original outright is exactly one request, always.
 */
export function decideFirstRequest(
	width: number | null | undefined,
	height: number | null | undefined,
	platform: Platform
): FirstRequest {
	const cls = classify(width, height);
	if (cls === 'unknown') {
		return platform === 'desktop' ? { variant: 'original' } : { variant: null };
	}
	if (cls === 'small' && withinThumbBound(width as number, height as number)) {
		// The image IS within the thumbnail bound — the download is the original.
		return { variant: 'original' };
	}
	// small-but-long (> 1024 px long edge) OR large, both with known dims.
	if (platform === 'mobile') {
		// large → tap affordance (nothing now); small-but-long → thumb, no auto original.
		return cls === 'large' ? { variant: null } : { variant: 'thumb-md', upgrade: false };
	}
	return { variant: 'thumb-md', upgrade: true };
}

/**
 * THE FALLBACK DETECTOR — the load-bearing piece.
 *
 * A cell that requested `thumb-md` but got a bitmap whose long edge exceeds the
 * thumbnail bound was served the ORIGINAL (the server falls back to it when the
 * variant is absent — a fresh upload mid-derivation, or any format the pure-Go
 * processor doesn't derive: WebP / AVIF). Two distinct URLs would then decode
 * the original TWICE, so the background upgrade is skipped.
 *
 * Format-blind and dimension-free: it reads only the decoded bitmap, so it can't
 * rot when the server's decoder set changes.
 */
export function servedOriginal(
	naturalWidth: number | null | undefined,
	naturalHeight: number | null | undefined
): boolean {
	const w = usableDimension(naturalWidth) ? naturalWidth : 0;
	const h = usableDimension(naturalHeight) ? naturalHeight : 0;
	return Math.max(w, h) > THUMB_LONG_EDGE;
}
