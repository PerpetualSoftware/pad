// Redirect-target validation + query-string helpers shared between
// /login and /register. Both pages support a `?redirect=` query param
// that lets the OAuth-server `/oauth/authorize` endpoint (or any
// future inbound deep link) bounce the user back to the original
// destination after auth completes.
//
// Validation logic mirrors what the `/login` page used to inline:
// a bare `startsWith('/')` check is NOT enough because protocol-
// relative URLs like `//evil.example` and `/\evil.example` pass that
// check but are treated by browsers (and most server-side redirect
// handlers) as cross-origin destinations. Reject those explicitly.

export const DEFAULT_REDIRECT = '/console';

/**
 * Validate the `redirect` query param and return a safe relative path.
 *
 * Returns DEFAULT_REDIRECT (`/console`) for missing, malformed, or
 * potentially-cross-origin values. Callers can compare the return
 * value against DEFAULT_REDIRECT to detect "no real redirect requested"
 * without re-parsing.
 */
export function validateRedirect(raw: string | null | undefined): string {
	if (
		raw &&
		raw.startsWith('/') &&
		!raw.startsWith('//') &&
		!raw.startsWith('/\\')
	) {
		return raw;
	}
	return DEFAULT_REDIRECT;
}

/**
 * Build a query-string fragment encoding the redirect target.
 *
 * Returns an empty string when the target is the default destination
 * so the fragment composes cleanly into URLs that don't already need
 * a separator. Pass separator='&' when appending to a URL that
 * already has a query string (e.g. `/auth/github?force=1`).
 *
 * @param target - Already-validated redirect target (output of validateRedirect).
 * @param separator - '?' to start a new query string (default), '&' to append.
 */
export function redirectQueryFragment(
	target: string,
	separator: '?' | '&' = '?'
): string {
	if (target === DEFAULT_REDIRECT) return '';
	return `${separator}redirect=${encodeURIComponent(target)}`;
}
