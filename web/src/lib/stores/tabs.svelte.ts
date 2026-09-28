import { api } from '$lib/api/client';
import { onWorkspaceWrite } from '$lib/api/workspaceWrites';
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
 * LANDINGS (TASK-3279, PLAN-3002 U5). Every landing in a workspace outside
 * the open set opens it as an EPHEMERAL tab: `land()`, called by the workspace
 * layout whenever the workspace it shows changes. A deep link, a `/-/r/`
 * redirect (the server 302s to the item URL, so it arrives as a deep link), a
 * guest landing, an accepted invitation and a restore all end in that layout,
 * which is why one call covers them. The server keeps at most one ephemeral
 * tab, replacing the previous one in its position, and an ephemeral open of a
 * workspace that already has a row changes nothing, so a redundant landing is
 * harmless; the store skips it only to save the request.
 *
 * DURABILITY (PLAN-3002 Q9). An ephemeral tab is kept by a double-click or a
 * drag (TopBar), by "Keep open", or by any WRITE in its workspace. Writes are
 * reported by the API client (`onWorkspaceWrite`), which is the one place
 * every REST write passes through.
 *
 * LAST ROUTE. A workspace's last route lives on its tab row, and nowhere else.
 * The localStorage fallback TASK-3271 kept for workspaces with no row is
 * retired here, because a landing now opens a row: the first commit sweeps
 * any `pad-last-route-*` keys still in storage, and nothing reads or writes
 * them. A navigation is written once per burst, after the landing for that
 * workspace (if any) has answered, so the PATCH does not race the row it
 * needs. A PATCH that finds no row (closed on another device, access lost)
 * drops that route.
 */

/** The retired pre-U5 localStorage key prefix, kept only to sweep leftovers. */
const LEGACY_LAST_ROUTE_PREFIX = 'pad-last-route-';

// How long a navigation waits before its route is written, so a burst of
// navigations costs one PATCH.
export const LAST_ROUTE_WRITE_DELAY_MS = 750;

let tabs = $state<WorkspaceTab[]>([]);
let loaded = $state(false);
// Routes noted but not yet confirmed by a PATCH. Read ahead of the row, so a
// switcher href shows the route the user just left instead of the row's older
// value.
let pendingRoutes = $state<Record<string, string>>({});

// Ticket at dispatch, high-water mark at commit. See "WHICH RESPONSE COMMITS".
let dispatched = 0;
let committed = 0;

const writeTimers = new Map<string, ReturnType<typeof setTimeout>>();
// Landing opens in flight, by slug, so a second landing on the same workspace
// does not send a second POST and a route write can wait for its row.
const landings = new Map<string, Promise<void>>();
// Pins in flight, by slug, so a burst of writes sends one.
const pinning = new Set<string>();
let legacySwept = false;

function sweepLegacyRouteKeys() {
	if (legacySwept) return;
	legacySwept = true;
	try {
		for (const key of Object.keys(localStorage)) {
			if (key.startsWith(LEGACY_LAST_ROUTE_PREFIX)) localStorage.removeItem(key);
		}
	} catch {
		// Storage disabled: nothing to sweep.
	}
}

function hasRow(slug: string): boolean {
	return tabs.some((t) => t.slug === slug);
}

async function send(call: () => Promise<WorkspaceTabsResponse>): Promise<WorkspaceTabsResponse> {
	const isSameIdentity = authStore.identityFence();
	const ticket = ++dispatched;
	const resp = await call();
	if (isSameIdentity() && ticket > committed) {
		committed = ticket;
		tabs = resp.tabs;
		loaded = true;
		sweepLegacyRouteKeys();
	}
	return resp;
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
	const isSameIdentity = authStore.identityFence();
	// The row this PATCH needs may be the one a landing is opening right now.
	await landings.get(slug)?.catch(() => {});
	if (!isSameIdentity()) return;
	const route = pendingRoutes[slug];
	if (route === undefined) return;
	try {
		await send(() => api.workspaces.tabs.update(slug, { last_route: route }));
		if (isSameIdentity()) setRowRoute(slug, route);
	} catch {
		// No row (a 404: closed elsewhere, or the landing failed), a refused
		// route, or a network failure. Each loses this one route; the next
		// navigation writes again.
	} finally {
		if (isSameIdentity() && pendingRoutes[slug] === route) delete pendingRoutes[slug];
	}
}

async function pinOnWrite(slug: string) {
	const tab = tabs.find((t) => t.slug === slug);
	if (!tab?.ephemeral || pinning.has(slug)) return;
	pinning.add(slug);
	try {
		await send(() => api.workspaces.tabs.update(slug, { pin: true }));
	} catch {
		// The tab stays ephemeral; the next write tries again.
	} finally {
		pinning.delete(slug);
	}
}

onWorkspaceWrite((slug) => void pinOnWrite(slug));

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

	/**
	 * A landing in `slug`: open it as an ephemeral tab unless it is already in
	 * the open set. Skipped only when a committed list says it has a row;
	 * before the first list answers the POST is sent anyway, since the server
	 * leaves an existing row alone. A workspace the caller cannot see answers
	 * the workspace 404, which rejects here and opens nothing.
	 */
	land(slug: string): Promise<void> {
		if (loaded && hasRow(slug)) return Promise.resolve();
		const inFlight = landings.get(slug);
		if (inFlight) return inFlight;
		const p = send(() => api.workspaces.tabs.open(slug, true))
			.then(() => {})
			.finally(() => {
				if (landings.get(slug) === p) landings.delete(slug);
			});
		landings.set(slug, p);
		return p;
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
		return tabs.find((t) => t.slug === slug)?.last_route || null;
	},

	/**
	 * Record a navigation to `route` inside `slug`, written to its tab row
	 * after LAST_ROUTE_WRITE_DELAY_MS.
	 */
	noteRoute(slug: string, route: string) {
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
	landings.clear();
	pinning.clear();
});
