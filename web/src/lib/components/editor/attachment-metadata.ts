/**
 * Shared per-attachment metadata fetcher used by the editor's
 * attachment-* extensions to enrich what they render. The MIME and
 * size aren't carried in the markdown reference (just the UUID), so
 * we lean on the existing GET handler's HEAD response — Content-Type
 * and Content-Length give us everything we need without a new API.
 *
 * The promise cache is keyed by `${ws}:${uuid}`, so:
 *   - Repeated chips / images for the same attachment pay one HEAD
 *     between them.
 *   - Undo / redo / paste operations that reinstantiate the same
 *     NodeView reuse the in-flight or settled fetch.
 *   - The cache lives for the page lifetime — attachment metadata is
 *     immutable (the row is content-addressed; transforms produce
 *     NEW rows), so there's no staleness concern.
 *
 * Callers never see an exception: every failure is reported through the
 * discriminated result below, so a surface with no workspace context
 * (headless rendering / SSR) simply doesn't call this at all.
 */

/** Variants the download URL builder must support. Mirrors AttachmentImage. */
export type AttachmentVariant = 'thumb-sm' | 'thumb-md' | 'original';

/** URL builder injected by Editor.svelte at configure time. */
export type AttachmentUrlBuilder = (uuid: string, variant?: AttachmentVariant) => string;

export interface AttachmentMetadata {
	mime: string;
	size: number;
}

/**
 * The outcome of a metadata probe (PLAN-2392 DR-17).
 *
 * The three arms exist because callers need to tell "the row is gone"
 * apart from "the request didn't make it", and the old `null` return
 * collapsed both:
 *
 *   - `ok`        — the HEAD succeeded; `mime` / `size` are usable.
 *   - `missing`   — the server answered 404. AUTHORITATIVE: the row is
 *                   gone, and a caller may latch a permanent
 *                   missing-attachment placeholder on it. This is what
 *                   keeps editor undo from resurrecting a deleted
 *                   attachment as a live-looking node.
 *   - `transient` — any other non-2xx (5xx, 401/403 mid-session, a
 *                   proxy hiccup) or a network throw. Says NOTHING
 *                   about whether the row exists; callers keep whatever
 *                   they were showing and stay retryable.
 */
export type AttachmentMetadataResult =
	| ({ status: 'ok' } & AttachmentMetadata)
	| { status: 'missing' }
	| { status: 'transient' };

const cache = new Map<string, Promise<AttachmentMetadataResult>>();

/**
 * Fetch (or read from cache) the MIME + size for an attachment. The
 * server registers HEAD alongside GET (TASK-877); chi doesn't auto-
 * route HEAD on GET handlers, so this must use HEAD — a GET would
 * pull the entire blob across the wire.
 *
 * Caching is per-arm (PLAN-2392 DR-17). `ok` and `missing` are both
 * durable facts about a content-addressed row, so they're kept for the
 * page lifetime. A `transient` result is NOT — it's evicted the moment
 * it settles, so a blip can't make a live attachment look permanently
 * unreadable for the rest of the session. The entry is still installed
 * BEFORE the request settles, so concurrent callers for the same key
 * share one in-flight HEAD either way; only the settled failure is
 * dropped.
 */
export function fetchAttachmentMetadata(
	workspaceSlug: string,
	uuid: string,
	getDownloadUrl: AttachmentUrlBuilder
): Promise<AttachmentMetadataResult> {
	const key = `${workspaceSlug}:${uuid}`;
	const existing = cache.get(key);
	if (existing) return existing;
	const promise: Promise<AttachmentMetadataResult> = (async () => {
		try {
			const resp = await fetch(getDownloadUrl(uuid), {
				method: 'HEAD',
				credentials: 'same-origin'
			});
			if (resp.status === 404) return { status: 'missing' as const };
			if (!resp.ok) return { status: 'transient' as const };
			const ctype = resp.headers.get('content-type') ?? '';
			const mime = ctype.split(';')[0].trim();
			const len = parseInt(resp.headers.get('content-length') ?? '0', 10);
			return {
				status: 'ok' as const,
				mime,
				size: Number.isFinite(len) && len >= 0 ? len : 0
			};
		} catch {
			return { status: 'transient' as const };
		}
	})();
	cache.set(key, promise);
	// Evict a transient failure once it settles. The identity check keeps
	// this from deleting a NEWER entry installed by an invalidate-then-
	// refetch that raced this promise's resolution.
	void promise.then((result) => {
		if (result.status === 'transient' && cache.get(key) === promise) {
			cache.delete(key);
		}
	});
	return promise;
}

/**
 * Drop a single entry from the cache. Used after a transform
 * succeeds — the new attachment's UUID is fresh so it has no entry
 * yet, but the editor may have re-rendered the original into another
 * spot before the transform; clearing keeps stale dimensions /
 * indicators from leaking forward.
 */
export function invalidateAttachmentMetadata(workspaceSlug: string, uuid: string): void {
	cache.delete(`${workspaceSlug}:${uuid}`);
}

/**
 * Map a MIME type to its canonical short format name as the server's
 * Capabilities reports it ("png" / "jpeg" / "gif" / "bmp" / "tiff" /
 * "webp" / "avif" / "heic"). Returns `null` for non-image MIMEs and
 * unrecognized image MIMEs — callers treat null the same as "format
 * not supported by current processor", which is the safe fallback.
 */
export function mimeToFormat(mime: string): string | null {
	const m = mime.toLowerCase().trim();
	if (!m.startsWith('image/')) return null;
	const sub = m.slice('image/'.length);
	switch (sub) {
		case 'png':
		case 'gif':
		case 'bmp':
		case 'tiff':
		case 'webp':
		case 'avif':
		case 'heic':
		case 'heif':
			return sub === 'heif' ? 'heic' : sub;
		case 'jpeg':
		case 'jpg':
		case 'pjpeg':
			return 'jpeg';
		default:
			return null;
	}
}
