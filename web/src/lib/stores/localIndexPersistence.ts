// localIndexPersistence — IndexedDB write-behind layer for localIndex
// (PLAN-1343 / TASK-1356). Per DOC-1342 design decision #4: the
// in-RAM Svelte store is canonical; IDB is a hydration source on
// cold/warm boot and a write-behind cache for every mutation. The
// reader path is unaffected — consumers always go through localIndex,
// never through this module directly.
//
// Database shape: one IDB database per workspace, named
// `pad-local-index-{wsSlug}`. Two object stores:
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
export const LOCAL_INDEX_SCHEMA_VERSION = 1;

/** Result of a `hydrate()` call. Empty payload when there's no cache yet. */
export interface HydrateResult {
	items: ItemIndexRow[];
	cursor: string;
}

/** Shape of the single row stored in the `meta` store. */
interface MetaRow {
	key: 'sync';
	cursor: string;
	schemaVersion: number;
}

// Open IDB connections are cached per workspace. Reopening is a real
// cost (page-load latency) and only matters when the persistence layer
// is actually used — most of the test surface lives in memory.
const dbs = new Map<string, IDBPDatabase>();

function isSupported(): boolean {
	return typeof indexedDB !== 'undefined';
}

function dbName(ws: string): string {
	return `pad-local-index-${ws}`;
}

/**
 * Open the workspace's IDB database, creating object stores on
 * first run. Cached so subsequent calls reuse the connection.
 * Returns null on any storage failure — callers must treat that as
 * "no cache available, fall back to network".
 */
async function open(ws: string): Promise<IDBPDatabase | null> {
	if (!isSupported()) return null;
	const cached = dbs.get(ws);
	if (cached) return cached;

	try {
		// The version arg to openDB is the IDB-format version (used
		// for migrations). We pin it to 1 and use our own
		// `schemaVersion` row in `meta` for content-shape versioning.
		// That keeps schema bumps decoupled from idb library quirks.
		const db = await openDB(dbName(ws), 1, {
			upgrade(db) {
				if (!db.objectStoreNames.contains('items')) {
					db.createObjectStore('items', { keyPath: 'id' });
				}
				if (!db.objectStoreNames.contains('meta')) {
					db.createObjectStore('meta', { keyPath: 'key' });
				}
			},
		});
		dbs.set(ws, db);
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
export async function hydrate(ws: string): Promise<HydrateResult> {
	const empty: HydrateResult = { items: [], cursor: '0' };
	if (!isSupported()) return empty;

	const db = await open(ws);
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
			await wipe(ws);
			return empty;
		}

		const items = (await tx.objectStore('items').getAll()) as ItemIndexRow[];
		await tx.done.catch(() => undefined);
		return {
			items: items ?? [],
			cursor: meta?.cursor ?? '0',
		};
	} catch {
		return empty;
	}
}

/**
 * Replace the cursor + schemaVersion meta row. Cheap, called after
 * every cursor-advancing mutation. Schema version is written every
 * time so the row stays self-consistent — there's no separate path
 * to "set version" vs. "set cursor".
 */
export async function persistCursor(ws: string, cursor: string): Promise<void> {
	if (!isSupported()) return;
	const db = await open(ws);
	if (!db) return;
	try {
		await db.put('meta', {
			key: 'sync',
			cursor,
			schemaVersion: LOCAL_INDEX_SCHEMA_VERSION,
		} satisfies MetaRow);
	} catch {
		/* swallow — best-effort cache */
	}
}

/**
 * Upsert a batch of rows in a single transaction. Used by
 * `applyDelta`/`bootstrap` write-through. Batching matters when an
 * SSE flurry arrives — a single tx is much cheaper than N
 * one-shot puts.
 */
export async function persistUpserts(
	ws: string,
	rows: ItemIndexRow[],
): Promise<void> {
	if (!isSupported() || rows.length === 0) return;
	const db = await open(ws);
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
 * Delete rows by id (hard remove). Used by `localIndex.remove` for
 * 403 purge (TASK-1360) and any other hard-delete path. Soft deletes
 * stay in the cache as upserts with `deleted_at` populated — they
 * flow through `persistUpserts`.
 */
export async function persistRemovals(
	ws: string,
	ids: string[],
): Promise<void> {
	if (!isSupported() || ids.length === 0) return;
	const db = await open(ws);
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
export async function wipe(ws: string): Promise<void> {
	if (!isSupported()) return;
	const existing = dbs.get(ws);
	if (existing) {
		try {
			existing.close();
		} catch {
			/* swallow */
		}
		dbs.delete(ws);
	}
	try {
		await new Promise<void>((resolve) => {
			const req = indexedDB.deleteDatabase(dbName(ws));
			req.onsuccess = () => resolve();
			req.onerror = () => resolve();
			req.onblocked = () => resolve();
		});
	} catch {
		/* swallow */
	}
}
