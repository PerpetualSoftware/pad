// Last-used auth method hint (TASK-923).
//
// Stores the auth method (`password`, `github`, `google`) the user most
// recently used to sign in, in the browser's own localStorage. Read on the
// /login page to render a soft "you used X last time" banner so returning
// users don't have to re-decide which button to click.
//
// Privacy
// -------
// - Only the method *name* is stored — never an email, user ID, or token.
// - localStorage is per-origin and is never sent over the wire.
// - No cookie, no URL param, no server log entry.
//
// OAuth note
// ----------
// The OAuth handshake completes externally (provider → pad-cloud sidecar →
// pad backend creates the session and redirects), so the web client never
// receives a "OAuth succeeded" callback in JS. Instead, the /login page
// records the chosen method *speculatively* when the user clicks
// "Continue with GitHub/Google" — see `recordAuthMethod` callsites. If the
// user bails out at the provider's consent screen, the value still
// reflects "what the user tried last", which is the right answer for the
// banner ("you used GitHub last time" stays accurate).
//
// All reads/writes are wrapped in try/catch so SSR (no `window`),
// Safari private mode (localStorage throws on write), or a paranoid CSP
// can't break the auth pages.

export type AuthMethod = 'password' | 'github' | 'google';

const METHOD_KEY = 'pad_last_auth_method';
const AT_KEY = 'pad_last_auth_at';

function isAuthMethod(value: unknown): value is AuthMethod {
	return value === 'password' || value === 'github' || value === 'google';
}

/**
 * Record the auth method the user just used. Safe to call from any page
 * that completes an auth flow — failures are swallowed so a broken
 * localStorage cannot block the sign-in path.
 */
export function recordAuthMethod(method: AuthMethod): void {
	if (typeof window === 'undefined') return;
	try {
		window.localStorage.setItem(METHOD_KEY, method);
		window.localStorage.setItem(AT_KEY, String(Date.now()));
	} catch {
		// Disabled storage / private mode / quota — the banner is a nice-to-
		// have, never block auth on it.
	}
}

/**
 * Read the most recently recorded auth method from localStorage. Returns
 * `null` for first-time visitors, SSR, or any storage error. The
 * timestamp is exposed so callers can implement freshness gates later
 * without changing this signature; v1 callers can ignore it.
 */
export function getLastAuthMethod(): { method: AuthMethod; at: number } | null {
	if (typeof window === 'undefined') return null;
	try {
		const raw = window.localStorage.getItem(METHOD_KEY);
		if (!isAuthMethod(raw)) return null;
		const at = Number(window.localStorage.getItem(AT_KEY) ?? '');
		return { method: raw, at: Number.isFinite(at) ? at : 0 };
	} catch {
		return null;
	}
}

/**
 * Clear the recorded auth method. Reserved for a future "this isn't me"
 * affordance and for tests; not currently called from the auth pages.
 */
export function clearLastAuthMethod(): void {
	if (typeof window === 'undefined') return;
	try {
		window.localStorage.removeItem(METHOD_KEY);
		window.localStorage.removeItem(AT_KEY);
	} catch {
		// See recordAuthMethod — never throw from a hint helper.
	}
}
