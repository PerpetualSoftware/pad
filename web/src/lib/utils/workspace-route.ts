import { tabsStore } from '$lib/stores/tabs.svelte';
import { workspaceStore } from '$lib/stores/workspace.svelte';
import { authStore } from '$lib/stores/auth.svelte';

/**
 * The workspace's home path, `/<owner>/<slug>`, or undefined when there is no
 * owner name to build it from.
 *
 * owner_username comes from a LEFT JOIN on users and can be '' or absent (a
 * workspace with no owner, or whose owner is gone; BUG-4576, BUG-4780). Fall
 * back to the signed-in user only for a workspace with no owner or one they
 * own. With no owner name, build no link: an empty first segment would make
 * `//<slug>`, a link to another host, and a missing one `/undefined/<slug>`.
 *
 * A WorkspaceTab carries no owner_id, and a Workspace omits it when empty, so
 * an object without one is resolved against `workspaceStore.workspaces` by
 * slug. One not found there is not treated as the user's (no fallback).
 */
export function workspaceHomePath(ws: {
	slug: string;
	owner_username?: string;
	owner_id?: string;
	is_guest?: boolean;
}): string | undefined {
	if (ws.owner_username) return `/${ws.owner_username}/${ws.slug}`;
	let ownerId = ws.owner_id;
	let guest = !!ws.is_guest;
	if (ownerId === undefined) {
		const stored = workspaceStore.workspaces.find((w) => w.slug === ws.slug);
		if (!stored) return undefined;
		if (stored.owner_username) return `/${stored.owner_username}/${ws.slug}`;
		ownerId = stored.owner_id ?? '';
		guest = guest || !!stored.is_guest;
	}
	const mine = !guest && (!ownerId || ownerId === authStore.userId);
	const owner = mine ? authStore.user?.username : undefined;
	return owner ? `/${owner}/${ws.slug}` : undefined;
}

/**
 * Compute the URL to navigate to when "switching to" a workspace —
 * the user's last-visited route in that workspace if we have one,
 * falling back to the dashboard otherwise.
 *
 * Implements TASK-754 (workspace switcher: restore last route). The
 * route is read from `tabsStore.routeFor` (the workspace's tab row;
 * TASK-3271, and since TASK-3279 the row is the only store), and noted by
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
 * Anything failing the gauntlet returns the dashboard fallback. With no
 * owner path to build (see `workspaceHomePath`) it returns '/', and the saved
 * route is not consulted: the prefix test needs the owner path.
 *
 * Safe to call on the server (returns the fallback when `window` is
 * unavailable) so the same helper can be used in `href={...}` slots
 * without hydration warnings.
 */
export function workspaceRestoreTarget(ws: {
	slug: string;
	owner_username?: string;
	owner_id?: string;
	is_guest?: boolean;
}): string {
	const fallback = workspaceHomePath(ws);
	if (!fallback) return '/';
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
