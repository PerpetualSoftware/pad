// localIndexPersistence — IndexedDB write-behind layer for localIndex
// (PLAN-1343 / TASK-1356). Per DOC-1342 design decision #4: the
// in-RAM Svelte store is canonical; IDB is a hydration source on
// cold/warm boot and a write-behind cache for every mutation. The
// reader path is unaffected — consumers always go through localIndex,
// never through this module directly.
//
// Database shape: one IDB database per (user, workspace) pair, named
// `pad-local-index-{userId}-{wsSlug}`. Scoping by user is required —
// the cache is what the user could see last sync, and if a different
// user signs into the same browser, exposing the previous user's
// view would be a real correctness/permission leak. Anonymous /
// bootstrap-time callers (with no user id yet) use the `anon`
// namespace; those caches are independent of any signed-in user.
//
// Two object stores:
//
//   items  (keyPath: 'id')   — ItemIndexRow rows, keyed by item.id
//   meta   (keyPath: 'key')  — { key: 'sync', cursor, schemaVersion }
//
// SCHEMA_VERSION is the local equivalent of the Yjs schemaVersion in
// `web/src/lib/collab/schemaVersion.ts`. Bump it whenever the
// `ItemIndexRow` wire shape or store layout changes incompatibly —
// hydrators will see the mismatch on open and drop the persisted
// data, forcing a full /items-index resync. The server is the
// source of truth, so dropping the cache is always safe.
//
// All public functions never throw. Storage failures (Safari private
// mode, browser eviction, quota exceeded) degrade silently to
// in-memory only operation; the next bootstrap hits /items-index
// the normal way and the warm-load fast path simply skips.
//
// SSR-safe: every IDB call is gated on `typeof indexedDB !== 'undefined'`
// so SvelteKit's prerender / SSR phase doesn't blow up.

import { openDB, type IDBPDatabase } from 'idb';
import type { ItemIndexRow } from '$lib/types';

/**
 * SCHEMA_VERSION is the cache-shape contract. Bump it whenever the
 * `ItemIndexRow` skinny projection or this module's IDB layout changes
 * incompatibly — old clients reopening on a new build see the
 * mismatch and wipe their store, then re-bootstrap from
 * `/items-index`. Server truth (items.content) is never persisted
 * here, so a cache wipe loses nothing.
 */
export const LOCAL_INDEX_SCHEMA_VERSION = 3;

/** Result of a `hydrate()` call. Empty payload when there's no cache yet. */
export interface HydrateResult {
	items: ItemIndexRow[];
	cursor: string;
	includesUnparentedMetadata: boolean | null;
}

/** Shape of the single row stored in the `meta` store. */
interface MetaRow {
	key: 'sync';
	cursor: string;
	schemaVersion: number;
	includesUnparentedMetadata: boolean;
}

// Open IDB connections are cached per (user, workspace) pair. The
// map key matches `dbName()` so cache slots can't collide across
// user namespaces.
const dbs = new Map<string, IDBPDatabase>();

function isSupported(): boolean {
	return typeof indexedDB !== 'undefined';
}

/**
 * Encode a value so it's safe to embed in an IDB database name.
 * Names allow any UTF-16 string per spec, but URL-encoding keeps
 * the on-disk handle predictable in dev tools and avoids surprises
 * from exotic IDs.
 */
function safe(s: string): string {
	return encodeURIComponent(s);
}

function dbName(userId: string | null, ws: string): string {
	const ns = userId ? safe(userId) : 'anon';
	return `pad-local-index-${ns}-${safe(ws)}`;
}

function key(userId: string | null, ws: string): string {
	return dbName(userId, ws);
}

/**
 * Open the workspace's IDB database for the given user, creating
 * object stores on first run. Cached so subsequent calls reuse the
 * connection. Returns null on any storage failure — callers must
 * treat that as "no cache available, fall back to network".
 */
async function open(
	userId: string | null,
	ws: string,
): Promise<IDBPDatabase | null> {
	if (!isSupported()) return null;
	const k = key(userId, ws);
	const cached = dbs.get(k);
	if (cached) return cached;

	try {
		// The version arg to openDB is the IDB-format version (used
		// for migrations). We pin it to 1 and use our own
		// `schemaVersion` row in `meta` for content-shape versioning.
		// That keeps schema bumps decoupled from idb library quirks.
		const db = await openDB(dbName(userId, ws), 1, {
			upgrade(db) {
				if (!db.objectStoreNames.contains('items')) {
					db.createObjectStore('items', { keyPath: 'id' });
				}
				if (!db.objectStoreNames.contains('meta')) {
					db.createObjectStore('meta', { keyPath: 'key' });
				}
			},
		});
		dbs.set(k, db);
		return db;
	} catch {
		return null;
	}
}

/**
 * Read everything from IDB for a workspace. Returns the persisted
 * items + cursor, OR an empty result when:
 *   - IDB isn't supported (SSR, ancient browser),
 *   - the persisted schemaVersion doesn't match LOCAL_INDEX_SCHEMA_VERSION
 *     (in which case the store is also wiped as a side effect so the
 *     next persist write starts fresh),
 *   - opening or reading fails (storage error, quota issue).
 *
 * Never throws. The caller does the cold-path /items-index fetch when
 * `items` is empty.
 */
export async function hydrate(
	userId: string | null,
	ws: string,
): Promise<HydrateResult> {
	const empty: HydrateResult = { items: [], cursor: '0', includesUnparentedMetadata: null };
	if (!isSupported()) return empty;

	const db = await open(userId, ws);
	if (!db) return empty;

	try {
		const tx = db.transaction(['items', 'meta'], 'readonly');
		const meta = (await tx.objectStore('meta').get('sync')) as
			| MetaRow
			| undefined;

		// Schema-version mismatch is the "your local cache is from a
		// previous incompatible build" case. Drop everything and
		// signal an empty cache. The next bootstrap fully resyncs.
		if (meta && meta.schemaVersion !== LOCAL_INDEX_SCHEMA_VERSION) {
			await tx.done.catch(() => undefined);
			await wipe(userId, ws);
			return empty;
		}

		const items = (await tx.objectStore('items').getAll()) as ItemIndexRow[];
		await tx.done.catch(() => undefined);
		return {
			items: items ?? [],
			cursor: meta?.cursor ?? '0',
			includesUnparentedMetadata: meta?.includesUnparentedMetadata ?? null,
		};
	} catch {
		return empty;
	}
}

/**
 * Upsert a batch of rows in a single transaction. Used by
 * `applyDelta`/`bootstrap` write-through. Batching matters when an
 * SSE flurry arrives — a single tx is much cheaper than N
 * one-shot puts.
 */
export async function persistUpserts(
	userId: string | null,
	ws: string,
	rows: ItemIndexRow[],
): Promise<void> {
	if (!isSupported() || rows.length === 0) return;
	const db = await open(userId, ws);
	if (!db) return;
	try {
		const tx = db.transaction('items', 'readwrite');
		const store = tx.objectStore('items');
		// Fire-and-forget per-row put; the .done promise waits for them all.
		for (const row of rows) {
			store.put(row).catch(() => undefined);
		}
		await tx.done;
	} catch {
		/* swallow — best-effort cache */
	}
}

/**
 * Atomically advance a delta — write upserted rows AND the new
 * cursor in a single IDB transaction. If the tx fails or is aborted
 * (browser eviction, quota, tab freeze), nothing is written so the
 * persisted cursor never overshoots the persisted rows. The next
 * warm hydrate sees a consistent floor and `/items-changes?since=`
 * can pick up from there without skipping rows. Codex P2 (round 1)
 * caught the divergence risk of separate row/cursor writes.
 *
 * Soft deletes flow through `rows` (as upserts with `deleted_at`
 * populated). Hard removals are either passed as `removeIds` (so a
 * moved-out eviction lands in the SAME tx as the cursor advance and
 * can't resurrect on warm boot — BUG-1675) or, for paths without a
 * cursor advance, go through `persistRemovals`.
 */
export async function persistDelta(
	userId: string | null,
	ws: string,
	rows: ItemIndexRow[],
	cursor: string,
	includesUnparentedMetadata: boolean,
	removeIds: string[] = [],
): Promise<void> {
	if (!isSupported()) return;
	const db = await open(userId, ws);
	if (!db) return;
	try {
		const tx = db.transaction(['items', 'meta'], 'readwrite');
		const itemsStore = tx.objectStore('items');
		for (const row of rows) {
			itemsStore.put(row).catch(() => undefined);
		}
		for (const id of removeIds) {
			itemsStore.delete(id).catch(() => undefined);
		}
		tx.objectStore('meta')
			.put({
				key: 'sync',
				cursor,
				schemaVersion: LOCAL_INDEX_SCHEMA_VERSION,
				includesUnparentedMetadata,
			} satisfies MetaRow)
			.catch(() => undefined);
		await tx.done;
	} catch {
		/* swallow — best-effort cache */
	}
}

/**
 * Atomically REPLACE the persisted snapshot — clear the items store and
 * write the given rows + cursor in a single readwrite transaction. Used by
 * the projection-scope resync, which needs the persisted cache to exactly
 * mirror an authoritative re-fetch (rows dropped by a permission downgrade
 * must not survive) without the cross-tab hazard of `wipe()`: a
 * `deleteDatabase()` resolves on `onblocked` while another tab holds the DB
 * open, leaving the delete pending so a following reopen+write can queue
 * behind it indefinitely. A single transaction over the still-open
 * connection sidesteps that — the clear and the puts commit together (or not
 * at all), and no connection is ever torn down.
 */
export async function persistReplace(
	userId: string | null,
	ws: string,
	rows: ItemIndexRow[],
	cursor: string,
	includesUnparentedMetadata: boolean,
): Promise<void> {
	if (!isSupported()) return;
	const db = await open(userId, ws);
	if (!db) return;
	try {
		const tx = db.transaction(['items', 'meta'], 'readwrite');
		const itemsStore = tx.objectStore('items');
		// Queued before the puts; IDB executes requests against a store in
		// issue order, so the clear always lands first.
		itemsStore.clear().catch(() => undefined);
		for (const row of rows) {
			itemsStore.put(row).catch(() => undefined);
		}
		tx.objectStore('meta')
			.put({
				key: 'sync',
				cursor,
				schemaVersion: LOCAL_INDEX_SCHEMA_VERSION,
				includesUnparentedMetadata,
			} satisfies MetaRow)
			.catch(() => undefined);
		await tx.done;
	} catch {
		/* swallow — best-effort cache */
	}
}

/**
 * Delete rows by id (hard remove). Used by `localIndex.remove` for
 * 403 purge (TASK-1360) and any other hard-delete path. Soft deletes
 * stay in the cache as upserts with `deleted_at` populated — they
 * flow through `persistUpserts`.
 */
export async function persistRemovals(
	userId: string | null,
	ws: string,
	ids: string[],
): Promise<void> {
	if (!isSupported() || ids.length === 0) return;
	const db = await open(userId, ws);
	if (!db) return;
	try {
		const tx = db.transaction('items', 'readwrite');
		const store = tx.objectStore('items');
		for (const id of ids) {
			store.delete(id).catch(() => undefined);
		}
		await tx.done;
	} catch {
		/* swallow — best-effort cache */
	}
}

/**
 * Drop the IDB database entirely. Used by `localIndex.reset` (403
 * full-workspace purge / sign-out) and internally by `hydrate` when
 * the schemaVersion doesn't match. Closes the cached connection
 * first so the delete request isn't blocked by a still-open handle.
 */
export async function wipe(
	userId: string | null,
	ws: string,
): Promise<void> {
	if (!isSupported()) return;
	const k = key(userId, ws);
	const existing = dbs.get(k);
	if (existing) {
		try {
			existing.close();
		} catch {
			/* swallow */
		}
		dbs.delete(k);
	}
	try {
		await new Promise<void>((resolve) => {
			const req = indexedDB.deleteDatabase(dbName(userId, ws));
			req.onsuccess = () => resolve();
			req.onerror = () => resolve();
			req.onblocked = () => resolve();
		});
	} catch {
		/* swallow */
	}
}
