import { tabsStore } from '$lib/stores/tabs.svelte';

/**
 * Compute the URL to navigate to when "switching to" a workspace —
 * the user's last-visited route in that workspace if we have one,
 * falling back to the dashboard otherwise.
 *
 * Implements TASK-754 (workspace switcher: restore last route). The
 * route is read from `tabsStore.routeFor` (the workspace's tab row, else
 * the pre-U2 localStorage fallback — TASK-3271), and noted by
 * `[username]/[workspace]/+layout.svelte`'s route persistence effect.
 *
 * The saved value is treated as untrusted input — could be stale,
 * corrupt, or crafted. We canonicalize through `URL` and require:
 *   - same origin (rejects scheme/`//host` smuggling),
 *   - workspace prefix on the *normalized* pathname,
 *   - no traversal segments or percent-encoded chars in the path
 *     (the app never generates either; rejecting them blocks
 *     `%2e%2e/...` style escapes that could pass a naive prefix
 *     check but resolve elsewhere via `goto()` normalization).
 *
 * Anything failing the gauntlet returns the dashboard fallback.
 *
 * Safe to call on the server (returns the fallback when `window` is
 * unavailable) so the same helper can be used in `href={...}` slots
 * without hydration warnings.
 */
export function workspaceRestoreTarget(ws: { slug: string; owner_username?: string }): string {
	const fallback = `/${ws.owner_username}/${ws.slug}`;
	if (typeof window === 'undefined') return fallback;
	try {
		const saved = tabsStore.routeFor(ws.slug);
		if (!saved) return fallback;
		const normalized = new URL(saved, window.location.origin);
		const path = normalized.pathname;
		const sameOrigin = normalized.origin === window.location.origin;
		const inWorkspace = path === fallback || path.startsWith(fallback + '/');
		const cleanPath =
			!path.includes('/..') &&
			!path.includes('/./') &&
			!path.includes('//') &&
			!/%[0-9a-fA-F]{2}/.test(path);
		if (sameOrigin && inWorkspace && cleanPath) {
			return normalized.pathname + normalized.search + normalized.hash;
		}
	} catch {
		// URL parse failed — fall through.
	}
	return fallback;
}
