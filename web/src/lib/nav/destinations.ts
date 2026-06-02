/**
 * Shared nav-destinations source.
 *
 * Single source of truth for the workspace's primary navigation destinations
 * and active-state logic, consumed by BOTH the desktop Sidebar
 * (`components/layout/Sidebar.svelte`) and the mobile BottomNav + "More" sheet
 * (`components/layout/BottomNav.svelte`). Keeping it here prevents the two nav
 * surfaces from drifting apart.
 *
 * These are PURE helpers — no store access, no reactivity. Callers (the Svelte
 * components) own `wsPrefix` (from `workspaceStore`), the dynamic collection
 * list (from `collectionStore`), and guest filtering. See DR-1 / DR-3 on
 * PLAN-1694.
 */

export type NavKey =
	| 'dashboard'
	| 'insights'
	| 'roles'
	| 'activity'
	| 'starred'
	| 'tags'
	| 'settings';

export interface NavDestination {
	key: NavKey;
	/** Built from `wsPrefix` (e.g. `/dave/docapp/insights`). */
	href: string;
	icon: string;
	label: string;
	/** Hidden on guest (shared) workspaces — mirrors the Sidebar's guest gating. */
	guestHidden?: boolean;
}

/**
 * Path segments directly under `wsPrefix` that are NOT user collections.
 * Keep in sync with the reserved routes in the workspace `[username]/[workspace]`
 * tree. Used by {@link getActiveKey} to avoid mis-flagging a reserved page as a
 * collection.
 */
export const RESERVED_SLUGS = [
	'settings',
	'new',
	'library',
	'activity',
	'starred',
	'tags',
	'roles',
	'insights',
	''
] as const;

/**
 * The static primary destinations, in Sidebar order. Dynamic collections are
 * appended by the caller (from `collectionStore`). `settings` carries
 * `guestHidden` so callers can drop it on shared workspaces.
 */
export function getPrimaryDestinations(wsPrefix: string): NavDestination[] {
	return [
		{ key: 'dashboard', href: wsPrefix, icon: '📊', label: 'Dashboard' },
		{ key: 'insights', href: `${wsPrefix}/insights`, icon: '📈', label: 'Insights' },
		{ key: 'roles', href: `${wsPrefix}/roles`, icon: '🎭', label: 'Roles' },
		{ key: 'activity', href: `${wsPrefix}/activity`, icon: '📋', label: 'Activity' },
		{ key: 'starred', href: `${wsPrefix}/starred`, icon: '⭐', label: 'Starred' },
		{ key: 'tags', href: `${wsPrefix}/tags`, icon: '🏷', label: 'Tags' },
		{ key: 'settings', href: `${wsPrefix}/settings`, icon: '⚙', label: 'Settings', guestHidden: true }
	];
}

/**
 * The active nav key for the current path, or `collection:<slug>` for a
 * collection page, or `null` when nothing matches. Mirrors the per-route
 * `$derived` checks the Sidebar used to inline (Sidebar.svelte:32-53).
 */
export function getActiveKey(pathname: string, wsPrefix: string): string | null {
	if (!wsPrefix) return null;
	if (pathname === wsPrefix) return 'dashboard';
	if (pathname === `${wsPrefix}/insights`) return 'insights';
	if (pathname === `${wsPrefix}/roles`) return 'roles';
	if (pathname === `${wsPrefix}/activity`) return 'activity';
	if (pathname === `${wsPrefix}/starred`) return 'starred';
	if (pathname === `${wsPrefix}/tags` || pathname.startsWith(`${wsPrefix}/tags/`)) return 'tags';

	const prefix = `${wsPrefix}/`;
	if (pathname.startsWith(prefix)) {
		const slug = pathname.slice(prefix.length).split('/')[0];
		if (!(RESERVED_SLUGS as readonly string[]).includes(slug)) return `collection:${slug}`;
	}
	return null;
}

/** The active collection slug for the current path, or `null`. */
export function getActiveCollectionSlug(pathname: string, wsPrefix: string): string | null {
	const key = getActiveKey(pathname, wsPrefix);
	return key && key.startsWith('collection:') ? key.slice('collection:'.length) : null;
}
