// idb test harness (PLAN-2636 unit 1). Helpers the localIndex persistence
// regression tests build on. All of these run under the `idb` vitest project,
// whose setup (`setup-idb.ts`) installs a fresh fake-indexeddb per test.
//
// Design note (lead ruling on the PLAN-2636 trail): the localIndex cache
// races are SNAPSHOT-staleness, not read-staleness — the stale thing is the
// RAM snapshot handed to persistUpserts, never its own `get`, which always
// reads committed state. IDB serializes overlapping readwrite transactions,
// so those races reproduce with plain SEQUENTIAL calls; there is deliberately
// NO interleaving hook, and none is needed. (An awaited barrier between a
// get and a put would also fail on a real browser with
// TransactionInactiveError — a fake-indexeddb-only seam pinning behavior
// production can't have.) These helpers therefore give cross-tab CONNECTIONS
// and version-MIGRATION control, not scheduling control.

import { openDB, type IDBPDatabase } from 'idb';
import { vi } from 'vitest';
import type { ItemIndexRow } from '$lib/types';

/**
 * Mirrors localIndexPersistence.dbName EXACTLY so the harness addresses the
 * same database the module writes to — `pad-local-index-{ns}-{ws}`, where the
 * user namespace and workspace are `encodeURIComponent`-encoded and a null
 * user is the literal `anon` namespace. A drift here would silently point the
 * raw readers / second connection at a different database and make every
 * assertion vacuous, so it is kept byte-identical to the source.
 */
export function harnessDbName(userId: string | null, ws: string): string {
	const ns = userId ? encodeURIComponent(userId) : 'anon';
	return `pad-local-index-${ns}-${encodeURIComponent(ws)}`;
}

/**
 * Load a FRESH instance of the persistence module — `vi.resetModules()`
 * clears its module-level connection cache (`dbs`) so a previous test's
 * cached handle (pointing at the prior test's now-discarded IDBFactory)
 * can't leak in. Pair with the per-test fresh factory in setup-idb.ts for
 * full isolation. Returns the module's public surface.
 */
export async function loadPersistence(): Promise<
	typeof import('$lib/stores/localIndexPersistence')
> {
	vi.resetModules();
	return import('$lib/stores/localIndexPersistence');
}

/**
 * A SECOND raw connection to the same (user, workspace) database — the
 * "other tab". Used by the 2635 cross-tab pins: one connection persists an
 * unmerged snapshot while another holds the merged row. fake-indexeddb backs
 * both handles with one in-memory store within a single test context
 * (verified in the unit-1 spike), so writes through one are visible to the
 * other, exactly like two browser tabs.
 *
 * Opens at IDB format version 1 with the same stores localIndexPersistence
 * creates, so it never triggers an unexpected upgrade.
 */
export async function openSecondConnection(
	userId: string | null,
	ws: string,
): Promise<IDBPDatabase> {
	return openDB(harnessDbName(userId, ws), 1, {
		upgrade(db) {
			if (!db.objectStoreNames.contains('items')) {
				db.createObjectStore('items', { keyPath: 'id' });
			}
			if (!db.objectStoreNames.contains('meta')) {
				db.createObjectStore('meta', { keyPath: 'key' });
			}
		},
	});
}

/**
 * Create a v1-SHAPED database (the pre-unit-2 layout: `items` + `meta`, IDB
 * format version 1) and seed it, so unit 2's v1→v2 migration exercise can
 * reopen it under the new upgrade path and assert the old data survives. The
 * connection is closed before returning so the migration open isn't blocked.
 */
export async function seedV1Database(
	userId: string | null,
	ws: string,
	items: ItemIndexRow[] = [],
	meta?: { cursor: string; schemaVersion: number; includesUnparentedMetadata: boolean },
): Promise<void> {
	const db = await openDB(harnessDbName(userId, ws), 1, {
		upgrade(db) {
			db.createObjectStore('items', { keyPath: 'id' });
			db.createObjectStore('meta', { keyPath: 'key' });
		},
	});
	try {
		const tx = db.transaction(['items', 'meta'], 'readwrite');
		for (const row of items) tx.objectStore('items').put(row);
		if (meta) {
			tx.objectStore('meta').put({ key: 'sync', ...meta });
		}
		await tx.done;
	} finally {
		db.close();
	}
}

/**
 * Read the raw `items` store for a database, bypassing localIndexPersistence
 * entirely — the ground-truth assertion for what is actually persisted.
 */
export async function rawItems(userId: string | null, ws: string): Promise<ItemIndexRow[]> {
	const db = await openSecondConnection(userId, ws);
	try {
		return (await db.getAll('items')) as ItemIndexRow[];
	} finally {
		db.close();
	}
}

/** Raw read of a single persisted item by id (or undefined). */
export async function rawItem(
	userId: string | null,
	ws: string,
	id: string,
): Promise<ItemIndexRow | undefined> {
	const db = await openSecondConnection(userId, ws);
	try {
		return (await db.get('items', id)) as ItemIndexRow | undefined;
	} finally {
		db.close();
	}
}

/**
 * Open a database at a HIGHER IDB format version to create a new object
 * store, mirroring what unit 2's v1→v2 bump will do, and assert the pre-
 * existing data survived the upgrade. Returns the observed `oldVersion` so a
 * test can pin that the upgrade actually ran (vs a clean create).
 */
export async function openWithMigration(
	userId: string | null,
	ws: string,
	toVersion: number,
	newStore: string,
): Promise<{ oldVersion: number; db: IDBPDatabase }> {
	let oldVersion = -1;
	const db = await openDB(harnessDbName(userId, ws), toVersion, {
		upgrade(db, from) {
			oldVersion = from;
			if (!db.objectStoreNames.contains(newStore)) {
				db.createObjectStore(newStore, { keyPath: 'id' });
			}
		},
	});
	return { oldVersion, db };
}

/**
 * Assert the fail-safe degrade: an OLD build opening a database at format
 * version `oldVersion` after a newer build already upgraded it to a higher
 * version gets a VersionError (which localIndexPersistence catches → returns
 * null → that tab degrades to memory-only). Returns the caught error name.
 */
export async function versionErrorOnDowngradeOpen(
	userId: string | null,
	ws: string,
	oldVersion: number,
): Promise<string | null> {
	try {
		const db = await openDB(harnessDbName(userId, ws), oldVersion);
		db.close();
		return null;
	} catch (e) {
		return (e as Error)?.name ?? String(e);
	}
}
