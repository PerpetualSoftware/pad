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
 * Path prefixes the Go server owns directly — these are NOT SvelteKit
 * routes, so post-auth navigation MUST use a full-page `window.location`
 * navigation rather than `goto()`.
 *
 * See `internal/server/server.go`'s `setupRouter`: each entry below maps
 * to a chi route group (or middleware-gated route) registered BEFORE the
 * SvelteKit catch-all that serves the SPA. If post-auth code uses
 * SvelteKit's client-side `goto()` to navigate to one of these, the
 * router never finds a matching SPA route and falls into the
 * `[username]/[workspace]` catch-all, which renders "No dashboard data
 * available." That's BUG-1083 — an unauthenticated user hitting
 * `/oauth/authorize` got bounced through `/login` and on successful
 * sign-in the SPA tried to handle the `/oauth/authorize?...` redirect
 * itself, parsing it as `username="oauth"`, `workspace="authorize"`.
 *
 * Keep this list in sync with the route groups mounted before the SPA
 * catch-all. New entries belong here whenever a Go-only route prefix is
 * added that a `?redirect=` flow might legitimately target.
 */
const SERVER_OWNED_PATH_PREFIXES = [
	'/oauth/', // /oauth/authorize, /oauth/token, /oauth/register, etc. (PLAN-943)
	'/api/', // REST API + SSE
	'/.well-known/', // OAuth + MCP discovery docs
	'/mcp', // MCP transport endpoints (cloud mode)
	'/metrics' // Prometheus scrape endpoint
];

/**
 * Returns true when `path` targets a route the Go server handles
 * directly (and the SvelteKit SPA does NOT have a matching route for).
 *
 * Callers should prefer `navigateToRedirectTarget` over hand-rolling the
 * branch — that helper picks the right navigation primitive for both
 * cases.
 *
 * Pure path comparison; does not parse the URL or look at the query
 * string. Inputs MUST already be validated against `validateRedirect`
 * (i.e. start with `/`, not `//`, not `/\`).
 */
export function isServerOwnedPath(path: string): boolean {
	if (!path || !path.startsWith('/')) return false;
	for (const prefix of SERVER_OWNED_PATH_PREFIXES) {
		if (path === prefix || path.startsWith(prefix)) return true;
		// Allow the bare prefix without trailing slash too — `/mcp` and
		// `/metrics` are exact endpoints, not prefixes, so check for an
		// exact match against the trimmed form. `/oauth` (no trailing
		// slash, no query) is NOT a real route and would 404, but treat
		// it as server-owned for safety: a 404 from the Go side is a
		// better failure mode than the SPA catch-all rendering an empty
		// dashboard for `username="oauth"`.
		const trimmed = prefix.endsWith('/') ? prefix.slice(0, -1) : prefix;
		if (path === trimmed || path.startsWith(trimmed + '?')) return true;
	}
	return false;
}

/**
 * Navigate to a post-auth redirect target.
 *
 * Picks `window.location.replace` for paths the Go server owns
 * (`/oauth/...`, `/api/...`, etc.) and SvelteKit's `goto()` for SPA
 * routes. `replace` (vs `assign`) matches the existing
 * `goto(target, { replaceState: true })` semantics — the user should
 * not be able to back-button into `/login` after a successful sign-in.
 *
 * Why not always use `window.location.replace`: the SPA's session +
 * workspace stores are pre-loaded by the time post-auth code runs, so
 * staying within the SPA for SPA-bound destinations skips a hard
 * reload (faster, fewer round-trips, preserves any hydrated state).
 *
 * Returns a promise so callers can `await` it; the server-owned branch
 * resolves immediately because `window.location.replace` doesn't
 * actually return until navigation tears down the page.
 */
export async function navigateToRedirectTarget(target: string): Promise<void> {
	if (isServerOwnedPath(target)) {
		if (typeof window !== 'undefined') {
			window.location.replace(target);
		}
		return;
	}
	// Lazy-import goto so this helper is safe to call from non-SvelteKit
	// contexts (e.g. unit tests, SSR tooling) — `$app/navigation` errors
	// out at import time outside a SvelteKit page module.
	const { goto } = await import('$app/navigation');
	await goto(target, { replaceState: true });
}

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
