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
 * Per-call fetch overrides (PLAN-2392 3c-ii T6). The one knob is `cache`, the
 * standard `RequestInit.cache` mode: a forced existence probe passes
 * `'no-store'` to bypass the browser HTTP cache (the endpoint sets
 * `max-age=3600`), so it actually reaches the server rather than replaying a
 * cached HEAD. Absent → fetch's default cache mode, i.e. the plain seed-fill
 * HEAD is untouched.
 */
export interface MetadataFetchOptions {
	cache?: RequestCache;
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
	getDownloadUrl: AttachmentUrlBuilder,
	options?: MetadataFetchOptions
): Promise<AttachmentMetadataResult> {
	const key = `${workspaceSlug}:${uuid}`;
	const existing = cache.get(key);
	if (existing) return existing;
	const promise: Promise<AttachmentMetadataResult> = (async () => {
		try {
			const init: RequestInit = { method: 'HEAD', credentials: 'same-origin' };
			// A forced existence probe passes `cache: 'no-store'` so the BROWSER
			// HTTP cache cannot answer from the GET/HEAD's `Cache-Control:
			// max-age=3600` (handlers_attachments.go). The promise cache above is
			// per-page state we control; the HTTP cache is not, and a cached 200
			// would hide a cross-tab / another-job delete — the exact thing an
			// existence probe exists to catch (PLAN-2392 3c-ii T6). Only set when a
			// caller asks: an unspecified `cache` leaves fetch on its default, so the
			// plain seed-fill HEAD is unchanged.
			if (options?.cache) init.cache = options.cache;
			const resp = await fetch(getDownloadUrl(uuid), init);
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
 * Ask the server about this attachment RIGHT NOW, ignoring anything
 * already cached.
 *
 * `fetchAttachmentMetadata` answers "what is this attachment?" and a
 * cached `ok` is a perfectly good answer — MIME and size are durable
 * facts about a content-addressed row. But an EXISTENCE probe asks a
 * different question, "is this row still there?", and a page-lifetime
 * cache structurally cannot answer it: the cached `ok` is a memory of
 * an earlier observation, so a row deleted since would still read as
 * live and a permanent placeholder would never latch (found by the
 * orchestrator's Codex pass on TASK-2420).
 *
 * The callers that need this are the ones holding contrary evidence —
 * an <img> whose load just failed, or a user pressing Retry (DR-10,
 * which requires invalidating before refetching for exactly this
 * reason). Use `fetchAttachmentMetadata` for everything else; this one
 * costs a round trip every call by design.
 */
export function revalidateAttachmentMetadata(
	workspaceSlug: string,
	uuid: string,
	getDownloadUrl: AttachmentUrlBuilder,
	options?: MetadataFetchOptions
): Promise<AttachmentMetadataResult> {
	invalidateAttachmentMetadata(workspaceSlug, uuid);
	return fetchAttachmentMetadata(workspaceSlug, uuid, getDownloadUrl, options);
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
