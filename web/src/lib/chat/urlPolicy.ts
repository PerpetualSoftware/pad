// URL policy for Chat message bodies (TASK-4315 PR 2a). Pure functions, shared by
// chat/markdown.ts (which builds the HTML) and the Chat profile of
// utils/markdown.ts::sanitizeMarkdownHtml (which enforces the same rules on the
// output, so a hole in either layer is closed by the other). Everything here
// takes the page origin as an argument so it is testable and has no globals.

/** Control characters, whitespace, DEL..C1, line/paragraph separators and backslash. */
const FORBIDDEN = /[\u0000-\u0020\u007f-\u009f\u2028\u2029\\]/;
const HAS_SCHEME = /^[a-zA-Z][a-zA-Z0-9+.\-]*:/;

const SLUG = '[a-z0-9](?:[a-z0-9-]*[a-z0-9])?';
const UUID = '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}';
const ATTACHMENT_PATH = new RegExp(`^/api/v1/workspaces/${SLUG}/attachments/${UUID}$`, 'i');
const ATTACHMENT_QUERY = /^(?:|\?variant=(?:thumb-sm|thumb-md|original))$/;

/**
 * An ordinary link target: an absolute http(s) URL, or a path-absolute,
 * protocol-relative, query or fragment reference resolved against `origin`.
 * Returns the normalised URL (path-relative when it is on `origin`), or null. Credentials, unknown schemes
 * (javascript:, data:, blob:, mailto: ...) and control characters are refused;
 * so is any relative reference that does not start with / ? # . (an entity or
 * percent-encoded scheme is not "relative", it is an obfuscation attempt).
 */
export function safeLinkHref(raw: string, origin: string): string | null {
	if (!raw || FORBIDDEN.test(raw)) return null;
	if (HAS_SCHEME.test(raw)) {
		if (!/^https?:/i.test(raw)) return null;
	} else if (!/^[/?#.]/.test(raw)) {
		return null;
	}
	let u: URL;
	try {
		u = new URL(raw, origin + '/');
	} catch {
		return null;
	}
	if (u.protocol !== 'http:' && u.protocol !== 'https:') return null;
	if (u.username !== '' || u.password !== '') return null;
	// A same-origin target stays path-relative (the server redirect routes read best that way).
	return u.origin === origin ? u.pathname + u.search + u.hash : u.href;
}

/**
 * An image source Chat may fetch: exactly Pad's own attachment route on the page
 * origin, `/api/v1/workspaces/{slug}/attachments/{uuid}` with at most one
 * `?variant=thumb-sm|thumb-md|original`. Anything else (another origin, encoded
 * separators, dot segments, extra query, a fragment, a suffix, credentials) is
 * null. The raw text must equal the parsed path+query, which rules out every
 * form the URL parser would silently normalise.
 */
export function attachmentImageSrc(raw: string, origin: string): string | null {
	if (!raw || FORBIDDEN.test(raw) || raw.includes('%')) return null;
	let rel: string;
	if (raw.startsWith('/') && !raw.startsWith('//')) rel = raw;
	else if (raw.startsWith(origin + '/')) rel = raw.slice(origin.length);
	else return null;
	let u: URL;
	try {
		u = new URL(rel, origin + '/');
	} catch {
		return null;
	}
	if (u.origin !== origin || u.username !== '' || u.password !== '') return null;
	if (u.pathname + u.search !== rel) return null;
	if (u.hash !== '' || !ATTACHMENT_PATH.test(u.pathname) || !ATTACHMENT_QUERY.test(u.search)) return null;
	return rel;
}
