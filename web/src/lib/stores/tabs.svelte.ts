import { api, PadApiError } from '$lib/api/client';
import type { WorkspaceTab, WorkspaceTabsResponse } from '$lib/types';
import { authStore } from './auth.svelte';

/**
 * The caller's open set of workspace tabs (PLAN-3002 U2 / TASK-3271), over
 * `/api/v1/me/workspace-tabs` (U1 / TASK-3256), plus each workspace's
 * last-visited route.
 *
 * WHICH RESPONSE COMMITS. Every write answers with the whole
 * visibility-filtered set, so every request here, read or write, REPLACES the
 * list. Two requests in flight can therefore answer out of order, and the older
 * one must not win: a list issued before an open answers without the new tab,
 * and committing it after the open's answer would erase that tab until the
 * next load. This is BUG-2981's create-vs-load race, and here it covers every
 * write rather than one, which is why it is not fenced the way
 * `workspace.svelte.ts` fences it (a pending-create reconcile). A ticket is
 * taken at dispatch and a high-water mark records the newest ticket that
 * COMMITTED. A response commits only above the mark, so the newest answer wins
 * and a request that fails never locks out one that succeeds.
 *
 * WHOSE RESPONSE COMMITS. The tickets order requests from one identity. A
 * response issued before a sign-out, sign-in or account switch must not commit
 * into the next identity's store (BUG-2991), so every request also captures
 * `authStore.identityFence()`. That is an epoch, not a user id, so A, then B,
 * then A again still drops a response from A's first session. The identity
 * reset below also raises the mark past every ticket issued so far, which
 * drops those responses by the ordering rule too. The two are REDUNDANT, and
 * the mutation matrix says so: removing either alone fails no test, removing
 * both fails the two identity tests. Both stay because each is a property of a
 * different mechanism, and either could be changed by someone who never reads
 * the other.
 *
 * LAST ROUTE (lead ruling on TASK-3271: option (b), until U5). A workspace's
 * last route is stored on its tab row, and U1 refuses a route for a workspace
 * with no row. Until U5 opens a tab at every landing, a workspace can be
 * visited without having a row, so the pre-U2 localStorage key is kept as the
 * fallback for exactly those workspaces:
 *   - reading: the tab row's route, then the localStorage key, then none (the
 *     caller's dashboard fallback). A read never waits on the network, so
 *     before the first load answers every workspace reads as having no row.
 *   - writing: one place per navigation, never both. PATCH when the workspace
 *     has a row, localStorage when it does not.
 *   - migration: a key is moved to its row, and removed, only once the
 *     workspace has a row. That includes a row opened later in the session,
 *     so migration runs after every commit, not only after the first load.
 *     Every other key is left alone.
 */

/** The localStorage key for a workspace's last route. Pinned by identityReload's test. */
export function lastRouteKey(slug: string): string {
	return `pad-last-route-${slug}`;
}

// How long a navigation waits before its route is written, so a burst of
// navigations costs one PATCH.
export const LAST_ROUTE_WRITE_DELAY_MS = 750;

let tabs = $state<WorkspaceTab[]>([]);
let loaded = $state(false);
// Routes noted for a workspace WITH a row that have not been confirmed by a
// PATCH yet. Read ahead of the row, so a switcher href shows the route the
// user just left instead of the row's older value.
let pendingRoutes = $state<Record<string, string>>({});

// Ticket at dispatch, high-water mark at commit. See "WHICH RESPONSE COMMITS".
let dispatched = 0;
let committed = 0;

const writeTimers = new Map<string, ReturnType<typeof setTimeout>>();
// Workspaces whose key is being moved to their row right now, so a second
// commit during the PATCH does not start a second one.
const migrating = new Set<string>();
// Workspaces whose route was written to localStorage BEFORE the first load
// answered. That key is newer than anything the row holds, so migration moves
// it to the row instead of discarding it in the row's favour.
const notedBeforeLoad = new Set<string>();

function readKey(slug: string): string | null {
	try {
		return localStorage.getItem(lastRouteKey(slug));
	} catch {
		return null;
	}
}

function writeKey(slug: string, route: string | null) {
	try {
		if (route) localStorage.setItem(lastRouteKey(slug), route);
		else localStorage.removeItem(lastRouteKey(slug));
	} catch {
		// Storage disabled: route memory just does not survive a reload.
	}
}

function hasRow(slug: string): boolean {
	return tabs.some((t) => t.slug === slug);
}

function isNotFound(err: unknown): boolean {
	return err instanceof PadApiError && err.code === 'not_found';
}

// The server refused the route itself (outside the workspace, malformed), and
// will refuse it again.
function isRouteRefused(err: unknown): boolean {
	return err instanceof PadApiError && (err.code === 'validation_error' || err.code === 'bad_request');
}

async function send(call: () => Promise<WorkspaceTabsResponse>): Promise<WorkspaceTabsResponse> {
	const isSameIdentity = authStore.identityFence();
	const ticket = ++dispatched;
	const resp = await call();
	if (isSameIdentity() && ticket > committed) {
		committed = ticket;
		tabs = resp.tabs;
		loaded = true;
		migrateKeys();
	}
	return resp;
}

function migrateKeys() {
	for (const tab of tabs) {
		const key = readKey(tab.slug);
		if (key === null || migrating.has(tab.slug)) continue;
		if (tab.last_route && !notedBeforeLoad.has(tab.slug)) {
			// The row already has a route, set on some device since the key was
			// written. The row is the newer record.
			writeKey(tab.slug, null);
			continue;
		}
		void migrateKey(tab.slug, key);
	}
	notedBeforeLoad.clear();
}

async function migrateKey(slug: string, route: string) {
	migrating.add(slug);
	const isSameIdentity = authStore.identityFence();
	try {
		await send(() => api.workspaces.tabs.update(slug, { last_route: route }));
		if (isSameIdentity()) setRowRoute(slug, route);
	} catch (err) {
		// A refused route will be refused again, so the key is spent. Anything
		// else keeps it: a 404 means the row went away, which makes the key the
		// workspace's fallback again, and any other failure retries at the
		// next commit.
		if (!isRouteRefused(err)) return;
	} finally {
		migrating.delete(slug);
	}
	if (!isSameIdentity()) return;
	// Only the value that was moved. A navigation during the PATCH with no row
	// cannot have written the key, since the row exists, but a sibling browser
	// tab still on the old code can.
	if (readKey(slug) === route) writeKey(slug, null);
}

// Record a route the server confirmed on the committed row, whether or not
// that PATCH's own response committed: a response issued later that answered
// first can hold the row's older value, and pending is about to be cleared.
function setRowRoute(slug: string, route: string) {
	const tab = tabs.find((t) => t.slug === slug);
	if (tab) tab.last_route = route || undefined;
}

async function flushRoute(slug: string) {
	const timer = writeTimers.get(slug);
	if (timer !== undefined) clearTimeout(timer);
	writeTimers.delete(slug);
	const route = pendingRoutes[slug];
	if (route === undefined) return;
	const isSameIdentity = authStore.identityFence();
	try {
		await send(() => api.workspaces.tabs.update(slug, { last_route: route }));
		if (isSameIdentity()) setRowRoute(slug, route);
	} catch (err) {
		if (!isSameIdentity()) return;
		if (isNotFound(err)) {
			// The row went away (closed on another device, access lost). The
			// workspace has no row now, which is the localStorage case.
			writeKey(slug, route);
			void tabsStore.load().catch(() => {});
		}
		// Any other failure loses this one route; the next navigation writes again.
	} finally {
		if (isSameIdentity() && pendingRoutes[slug] === route) delete pendingRoutes[slug];
	}
}

export const tabsStore = {
	/** The open set, in bar order. Empty until the first load answers. */
	get tabs() { return tabs; },
	/** True once a list has committed for this identity. */
	get loaded() { return loaded; },

	async load(): Promise<void> {
		await send(() => api.workspaces.tabs.list());
	},

	/** Open a tab. An ephemeral open replaces the current ephemeral tab. */
	async open(slug: string, ephemeral = false): Promise<void> {
		await send(() => api.workspaces.tabs.open(slug, ephemeral));
	},

	async close(slug: string): Promise<void> {
		await send(() => api.workspaces.tabs.close(slug));
	},

	/** The full order, as slugs. */
	async reorder(slugs: string[]): Promise<void> {
		await send(() => api.workspaces.tabs.reorder(slugs));
	},

	/** Keep an ephemeral tab. */
	async pin(slug: string): Promise<void> {
		await send(() => api.workspaces.tabs.update(slug, { pin: true }));
	},

	/**
	 * The last route stored for `slug`, or null. Synchronous: never waits on
	 * the network. The value is untrusted; `workspaceRestoreTarget` validates
	 * it before it becomes an href.
	 */
	routeFor(slug: string): string | null {
		const pending = pendingRoutes[slug];
		if (pending !== undefined) return pending || null;
		const tab = tabs.find((t) => t.slug === slug);
		if (tab?.last_route) return tab.last_route;
		return readKey(slug);
	},

	/**
	 * Record a navigation to `route` inside `slug`. Written after
	 * LAST_ROUTE_WRITE_DELAY_MS to the row when the workspace has one, else
	 * immediately to localStorage.
	 */
	noteRoute(slug: string, route: string) {
		if (!hasRow(slug)) {
			writeKey(slug, route);
			if (!loaded) notedBeforeLoad.add(slug);
			return;
		}
		pendingRoutes[slug] = route;
		const prior = writeTimers.get(slug);
		if (prior !== undefined) clearTimeout(prior);
		writeTimers.set(
			slug,
			setTimeout(() => void flushRoute(slug), LAST_ROUTE_WRITE_DELAY_MS),
		);
	},

	/**
	 * Replace `slug`'s stored route now, or clear it with null. For a repair
	 * (an item the route pointed at is gone), not for navigation.
	 */
	setRoute(slug: string, route: string | null) {
		if (!hasRow(slug)) {
			writeKey(slug, route);
			return;
		}
		pendingRoutes[slug] = route ?? '';
		void flushRoute(slug);
	},
};

// Everything here belongs to one identity. In-flight responses are dropped by
// their own fence and by the raised mark; pending writes are dropped outright,
// since they were routes the previous identity visited.
authStore.onIdentityChange(() => {
	committed = dispatched;
	tabs = [];
	loaded = false;
	pendingRoutes = {};
	for (const timer of writeTimers.values()) clearTimeout(timer);
	writeTimers.clear();
	migrating.clear();
	notedBeforeLoad.clear();
});
