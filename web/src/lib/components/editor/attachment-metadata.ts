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
 *     NEW rows), so a settled `ok` cannot go stale.
 *
 * A settled `missing` CAN, and that is not a caveat on the above but a
 * different fact: it describes REACHABILITY, not the row's contents.
 * Archiving the parent item 404s its attachments without deleting them
 * (DR-13), so a `missing` observed in that window expires when the item is
 * restored — see `invalidateAttachmentMetadataForWorkspace` (BUG-2509).
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
	/**
	 * The derived (thumbnail) variants the SERVER holds for this attachment —
	 * parsed from the `X-Pad-Attachment-Derived` response header (BUG-2964).
	 *
	 * A LIST, not a boolean (codex round 1): derivation writes each variant
	 * independently, so a consumer has to ask about the variant IT will request,
	 * not about whether any thumbnail at all exists.
	 *
	 * THREE-VALUED, and the third value is the whole point:
	 *
	 *  - `['thumb-sm','thumb-md']` — these exist.
	 *  - `[]`        — the header said `none`; this build derived nothing for
	 *                  this file, so the only bytes on offer are the original.
	 *  - `'unknown'` — no header at all, i.e. a server predating BUG-2964.
	 *                  Callers must fall back to their previous behaviour here;
	 *                  treating it as `[]` would flip every embed on an older
	 *                  server, which is a far bigger change than the bug.
	 */
	derived: string[] | 'unknown';
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
 * Is this result a DURABLE fact worth keeping for the page's lifetime?
 *
 * `mime` and `size` are durable — the row is content-addressed. `derived` is
 * NOT, and conflating the two is a real defect (BUG-2964, codex round 2):
 * thumbnail derivation runs ASYNCHRONOUSLY after upload, so a probe issued in
 * that window sees no variants yet. Caching that answer as immutable latches it
 * for the rest of the page — and on a build that CAN derive HEIC, a freshly
 * uploaded HEIC would then render as a file chip until a reload, even though
 * the thumbnail landed a second later.
 *
 * So an EMPTY derived list is provisional and is not cached; the next probe
 * asks again. A NON-EMPTY list is durable (variants are never un-derived), and
 * `'unknown'` is durable too — it is a fact about the SERVER's build, not about
 * this attachment, and it will not change under a running page.
 *
 * The cost is one repeat HEAD per probe for a file this build will never derive
 * — a HEIC on a pure-Go instance. That is bounded, cheap (HEAD, and the
 * endpoint is conditional-request friendly), and strictly better than latching
 * a wrong answer: the alternative trades a permanent visible error for a
 * request nobody notices.
 */
function isDurable(result: AttachmentMetadataResult): boolean {
	// `missing` stays DURABLE — it is authoritative by design (DR-17), and it is
	// what keeps editor undo from resurrecting a deleted attachment. Only
	// `transient` and a provisional empty `derived` are evicted; the first draft
	// of this helper demoted `missing` along with them, and three existing legs
	// caught it.
	if (result.status === 'transient') return false;
	if (result.status !== 'ok') return true;
	return result.derived === 'unknown' || result.derived.length > 0;
}


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
			// `none` is a SENTINEL, not an empty value: an empty header value is
			// the one a proxy may drop, and absence has to keep meaning "old
			// server" (see AttachmentMetadata.derived).
			const derivedHeader = resp.headers.get('x-pad-attachment-derived');
			return {
				status: 'ok' as const,
				mime,
				size: Number.isFinite(len) && len >= 0 ? len : 0,
				derived:
					derivedHeader === null
						? ('unknown' as const)
						: derivedHeader
								.split(',')
								.map((v) => v.trim())
								.filter((v) => v !== '' && v !== 'none')
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
		if (cache.get(key) === promise && !isDurable(result)) {
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
 * Drop every entry this workspace holds (BUG-2509).
 *
 * The module docstring's premise — a settled result is a durable fact about a
 * content-addressed row, so `ok` and `missing` are both safe to keep for the
 * page lifetime — is true for DELETION and false for ARCHIVE. Archiving the
 * PARENT item 404s its attachments (handlers_attachments.go, DR-13) without
 * touching the attachment rows, and restoring the parent brings them back. So a
 * `missing` observed inside the archived window is a fact WITH AN EXPIRY, cached
 * as though it had none: after the restore, every later reader of this cache —
 * including a NodeView constructed fresh, which is why remounting the editor did
 * not heal it — replays a 404 the server would no longer give.
 *
 * Called on the restore edge (`announceAttachmentParentRestored`), where the
 * negative knowledge is exactly what has to go. It drops `ok` entries too rather
 * than tracking per-entry status: this cache is a per-page memo, refilling it
 * costs one HEAD per visible attachment, and "drop everything for the workspace"
 * has no ordering hazard to get wrong. In-flight callers are unaffected — they
 * already hold the promise; only future lookups re-ask.
 *
 * It does NOT weaken the DR-17 undo guard. Dropping a deletion-derived `missing`
 * cannot resurrect anything, because every consumer re-probes and the SERVER is
 * what answers: a genuinely deleted row 404s again and re-latches. The cache was
 * never the authority — it was only ever a memo of one.
 */
export function invalidateAttachmentMetadataForWorkspace(workspaceSlug: string): void {
	if (!workspaceSlug) return;
	const prefix = `${workspaceSlug}:`;
	for (const key of [...cache.keys()]) {
		if (key.startsWith(prefix)) cache.delete(key);
	}
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
