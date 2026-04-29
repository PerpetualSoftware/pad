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
 * Skipped silently when no workspace context is available (e.g.
 * headless rendering / SSR) — callers see `null` and degrade UI
 * accordingly without raising errors.
 */

/** Variants the download URL builder must support. Mirrors AttachmentImage. */
export type AttachmentVariant = 'thumb-sm' | 'thumb-md' | 'original';

/** URL builder injected by Editor.svelte at configure time. */
export type AttachmentUrlBuilder = (uuid: string, variant?: AttachmentVariant) => string;

export interface AttachmentMetadata {
	mime: string;
	size: number;
}

const cache = new Map<string, Promise<AttachmentMetadata | null>>();

/**
 * Fetch (or read from cache) the MIME + size for an attachment. The
 * server registers HEAD alongside GET (TASK-877); chi doesn't auto-
 * route HEAD on GET handlers, so this must use HEAD — a GET would
 * pull the entire blob across the wire.
 */
export function fetchAttachmentMetadata(
	workspaceSlug: string,
	uuid: string,
	getDownloadUrl: AttachmentUrlBuilder
): Promise<AttachmentMetadata | null> {
	const key = `${workspaceSlug}:${uuid}`;
	const existing = cache.get(key);
	if (existing) return existing;
	const promise: Promise<AttachmentMetadata | null> = (async () => {
		try {
			const resp = await fetch(getDownloadUrl(uuid), {
				method: 'HEAD',
				credentials: 'same-origin'
			});
			if (!resp.ok) return null;
			const ctype = resp.headers.get('content-type') ?? '';
			const mime = ctype.split(';')[0].trim();
			const len = parseInt(resp.headers.get('content-length') ?? '0', 10);
			return { mime, size: Number.isFinite(len) && len >= 0 ? len : 0 };
		} catch {
			return null;
		}
	})();
	cache.set(key, promise);
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
