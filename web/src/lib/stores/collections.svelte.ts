import { api } from '$lib/api/client';
import type { Collection, Item } from '$lib/types';
import { authStore } from './auth.svelte';
import { hydrateCollections, persistCollections, readDurableEpoch } from './localIndexPersistence';
import { createKeyedSingleFlight } from './singleFlight';

let collections = $state<Collection[]>([]);
let items = $state<Item[]>([]);
// Workspace slug the current `items` array was loaded for. Used to decide
// whether `items` is stale for a different workspace (BUG-1461). `items` is
// a single global slot, so without this stamp a navigation from workspace A
// to workspace B would leave A's items in the store and freshness checks
// based purely on `items.length` would silently accept them as B's data —
// causing wiki-link resolution (which scans `items` for title/ref matches)
// to render `[[X]]` brackets as plain text. Null = no load completed yet
// for a full workspace; a non-null value pairs the items array with its
// source workspace.
let itemsWorkspace = $state<string | null>(null);
// Workspace slug the current `collections` array was loaded for — the
// collections analogue of `itemsWorkspace`. `collections` is likewise a
// single global slot that retains the previous workspace's data while
// `loadCollections()` for the new workspace is in flight. A consumer that
// pairs the collections with a specific `wsSlug` — e.g. TASK-2160's editor
// content-link gate maps collection slug → ref prefix and validates a link
// against the CURRENT workspace — must be able to tell "these collections
// are actually this workspace's" from "stale from the last workspace"
// (Codex review). Null = no load completed yet.
let collectionsWorkspace = $state<string | null>(null);
let activeItem = $state<Item | null>(null);
let loading = $state(false);

// The keyed single-flight loader that fences `loadCollections` (TASK-2947).
// Generation, join slot and ownership-guarded cleanup all live in
// `singleFlight.ts` now, shared with `workspace.svelte.ts` — the two stores
// hand-rolled the same three parts and drifted three times in one afternoon.
//
// Keyed by WORKSPACE SLUG, so a joiner asking about A is never handed B's
// promise. It never coalesces: `run` always issues, and only
// `ensureCollections` opts into joining via `inFlightFor`. See that method, and
// `singleFlight.ts`'s own note, for why every other call site must NOT join.
//
// `loading` stays a `$state` in this file rather than moving into the loader,
// because `loadItems` writes the same flag.
const collectionsFlight = createKeyedSingleFlight<string>({
	setLoading: (v) => { loading = v; },
});

export const collectionStore = {
	get collections() { return collections; },
	get items() { return items; },
	get itemsWorkspace() { return itemsWorkspace; },
	get collectionsWorkspace() { return collectionsWorkspace; },
	get activeItem() { return activeItem; },
	get loading() { return loading; },

	/**
	 * Returns true when `items` was last loaded as a full-workspace list
	 * (no collection filter) for the given workspace slug. Callers that
	 * need ALL items for wiki-link resolution should use this gate rather
	 * than `items.length === 0` — the latter can't distinguish "empty
	 * workspace" from "stale items from a different workspace."
	 */
	itemsAreFreshFor(ws: string): boolean {
		return itemsWorkspace === ws;
	},

	/**
	 * Returns true when the `collections` array was last loaded for the given
	 * workspace slug — the collections analogue of `itemsAreFreshFor`. A
	 * consumer that pairs collection metadata (e.g. slug → prefix) with a
	 * specific workspace must gate on this rather than trust a possibly-stale
	 * global array mid workspace-switch (Codex review, TASK-2160).
	 */
	collectionsAreFreshFor(ws: string): boolean {
		return collectionsWorkspace === ws;
	},

	get defaultCollections() {
		return collections.filter(c => c.is_default).sort((a, b) => a.sort_order - b.sort_order);
	},

	get customCollections() {
		return collections.filter(c => !c.is_default).sort((a, b) => a.sort_order - b.sort_order);
	},

	/**
	 * ENSURE this workspace's collection list exists — as distinct from
	 * `loadCollections`, which fetches a fresh one (TASK-2200).
	 *
	 * Two intents, and only one of them can be satisfied by a request that is
	 * already in flight:
	 *
	 *   - "I need A list" (this method). A request issued a moment ago answers
	 *     it perfectly, so joining it is right and a second fetch is waste.
	 *   - "I need a FRESH list" (`loadCollections`). The caller knows the list
	 *     MOVED — an SSE rename, a settings save, a server `collections_changed`
	 *     — and a request issued BEFORE that move cannot answer it. Coalescing
	 *     there would quietly serve pre-change data.
	 *
	 * That is why the coalescing lives here rather than inside `loadCollections`
	 * where it would cover every caller. Every other call site in the app is the
	 * second intent and is correctly left alone; this is the only one that is
	 * the first (codex round 2 asked for the population, and that is it).
	 *
	 * No-ops when the list is already this workspace's. Never rejects for the
	 * fresh-already case; a genuine fetch failure rejects like `loadCollections`.
	 */
	async ensureCollections(ws: string): Promise<void> {
		if (collectionsWorkspace === ws) return;
		// Join, rather than issue a second request for the same answer. The
		// joined promise settles when THAT request does, which is the semantics
		// the caller wants: "tell me when a list exists".
		const joined = collectionsFlight.inFlightFor(ws);
		if (joined) return joined;
		return collectionStore.loadCollections(ws);
	},

	/**
	 * The cached collection list for this workspace, or null (TASK-2946).
	 *
	 * For the caller that has just FAILED to reach the server and needs to
	 * render something honest. The scope fence lives in `hydrateCollections`,
	 * so this cannot hand back a list whose scope the durable cache disagrees
	 * with.
	 *
	 * DELIBERATELY NOT ADOPTED INTO `collections`. Seeding the reactive array
	 * from cache would also have to stamp `collectionsWorkspace`, and that
	 * stamp is what `collectionsAreFreshFor` answers — which TASK-2200's
	 * recovery reads to decide whether to keep re-fetching. A cached list
	 * marked fresh would stop the retry that is the only route back to a real
	 * one. So the cache is a read for a caller that wants it, not a substitute
	 * for the array; the sidebar stays empty until a fetch succeeds, and that
	 * is the correct trade rather than an oversight.
	 */
	async cachedCollection(ws: string, slug: string): Promise<Collection | null> {
		const list = await hydrateCollections(authStore.userId || null, ws);
		return list?.find((c) => c.slug === slug) ?? null;
	},

	async loadCollections(ws: string) {
		// The DURABLE scope claim before the request, so `persistCollections` can
		// tell whether a resync landed underneath it — the list carries no scope
		// of its own and the stamp is borrowed from the row cache, so it is only
		// honest if that cache held still (TASK-2946).
		//
		// Durable rather than `localIndex.accessEpochFor(ws)`, for two reasons
		// that both cost the feature its common path while it was RAM-based
		// (codex round 2): a cold tab has no RAM epoch yet, because this fetch
		// starts before `bootstrap` runs, so every fresh visit refused to cache;
		// and RAM is not the value `hydrateCollections` compares against, so a
		// RAM-based bracket could agree while the fence's own value moved.
		// THE NAMESPACE COMES FROM `authStore`, not from localIndex (codex round
		// 3). The cache is one IDB database per (user, workspace), and
		// localIndex's own `userId` is a copy captured when `bootstrap` ran —
		// which is AFTER the layout starts this fetch. Reading it here returned
		// null on exactly the first visit this feature exists for, writing the
		// list into the `anon` database while every later read used the
		// authenticated one. Same source as every `bootstrap` caller passes, so
		// the two cannot disagree.
		const userId = authStore.userId || null;
		// ALWAYS ISSUES — `run` never coalesces. The join is `ensureCollections`'s
		// alone, via `inFlightFor`, because every other caller here knows the list
		// MOVED and a request issued before that move cannot answer it.
		return collectionsFlight.run(ws, async ({ isLatest }) => {
			// STRICTLY BEFORE the request, not concurrently with it (codex round
			// 3). Running them together looked free and was not: `Promise.all`
			// starts both, but the durable read RESOLVES later and can observe a
			// resync that landed AFTER the request was issued. The write would
			// then compare that later epoch against itself, agree, and stamp an
			// old-scope list with the new scope — the same defeat-by-construction
			// round 1 found, reintroduced by the fix for round 2.
			//
			// The join `ensureCollections` performs does not depend on the fetch
			// being issued synchronously; it depends on the in-flight slot, which
			// `run` publishes synchronously before calling this.
			const epochBefore = await readDurableEpoch(userId, ws);
			const result = await api.collections.list(ws);
			// Drop a stale response: a newer loadCollections (e.g. a workspace
			// switch that resolved first) has superseded this one, so writing
			// `collections`/`collectionsWorkspace` here would clobber the newer
			// workspace's data with this older load (Codex review).
			if (!isLatest()) return;
			collections = result;
			// Stamp the array with its source workspace so consumers can tell
			// it apart from a stale previous-workspace load (see
			// `collectionsAreFreshFor`). Set only on success — a failed load
			// leaves the prior (possibly stale) array in place, and its stamp
			// with it, which is the correct conservative signal.
			collectionsWorkspace = ws;
			// Cache the list for a future cold load that cannot reach the server
			// (TASK-2946). Fire-and-forget and best-effort, like every other
			// durable write here; `persistCollections` itself decides whether the
			// stamp it would write is honest.
			void persistCollections(userId, ws, result, epochBefore);
		});
	},

	async loadItems(ws: string, collectionSlug?: string, params?: Record<string, string | number | boolean | undefined>) {
		loading = true;
		try {
			if (collectionSlug) {
				items = await api.items.listByCollection(ws, collectionSlug, params);
				// Partial load — the stored array no longer represents the
				// full workspace, so invalidate the freshness stamp. A
				// downstream caller asking `itemsAreFreshFor(ws)` will
				// correctly trigger a full re-load.
				itemsWorkspace = null;
			} else {
				items = await api.items.list(ws, params);
				itemsWorkspace = ws;
			}
		} finally {
			loading = false;
		}
	},

	async loadItem(ws: string, slug: string) {
		activeItem = await api.items.get(ws, slug);
		return activeItem;
	},

	setActiveItem(item: Item | null) {
		activeItem = item;
	},

	addItem(item: Item) {
		if (!items.find(i => i.id === item.id)) {
			items = [...items, item];
		}
	},

	updateItemInList(item: Item) {
		items = items.map(i => i.id === item.id ? item : i);
		if (activeItem?.id === item.id) {
			activeItem = item;
		}
	},

	removeItem(slug: string) {
		items = items.filter(i => i.slug !== slug);
		if (activeItem?.slug === slug) {
			activeItem = null;
		}
	},
};
