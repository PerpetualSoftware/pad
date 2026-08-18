import { describe, it, expect } from 'vitest';
import type { ItemIndexRow } from '$lib/types';
import {
	openSecondConnection,
	seedV1Database,
	openWithMigration,
	versionErrorOnDowngradeOpen,
	rawItem,
} from './idbHarness';

/**
 * PLAN-2636 unit 1 — capability pins for the two things the lead named as
 * blocking a harness shape (unit-2 test matrix prerequisites). These are
 * living characterizations, not just a one-off spike: if a fake-indexeddb
 * bump ever regressed either capability, unit 2's matrix would fail
 * mysteriously; here it fails at the harness with a clear message.
 */

function row(id: string, seq: number): ItemIndexRow {
	return { id, seq } as unknown as ItemIndexRow;
}

describe('cross-tab: two connections share one in-memory database (2635 prerequisite)', () => {
	it('a write through one connection is visible through a second connection', async () => {
		const U = null;
		const WS = 'ws-crosstab';
		const tabA = await openSecondConnection(U, WS);
		const tabB = await openSecondConnection(U, WS);
		try {
			await tabA.put('items', row('shared', 1));
			// tabB is a distinct handle; it must see tabA's committed write, the
			// way two browser tabs on one origin share the database.
			expect(((await tabB.get('items', 'shared')) as ItemIndexRow | undefined)?.seq).toBe(1);

			await tabB.put('items', row('shared', 2));
			expect(((await tabA.get('items', 'shared')) as ItemIndexRow | undefined)?.seq).toBe(2);
		} finally {
			tabA.close();
			tabB.close();
		}
	});
});

describe('format-version migration (unit-2 v1→v2 prerequisite)', () => {
	it('a v1 database reopens under a higher format version with items retained + new store created', async () => {
		const U = null;
		const WS = 'ws-migrate';
		await seedV1Database(U, WS, [row('keep', 3)], {
			cursor: 'c1',
			schemaVersion: 3,
			includesUnparentedMetadata: true,
		});

		const { oldVersion, db } = await openWithMigration(U, WS, 2, 'tombstones');
		try {
			// The upgrade actually ran from v1 (not a fresh create).
			expect(oldVersion).toBe(1);
			// Pre-existing items survived the format bump.
			expect(((await db.get('items', 'keep')) as ItemIndexRow | undefined)?.seq).toBe(3);
			// The new store exists.
			expect(db.objectStoreNames.contains('tombstones')).toBe(true);
			// And the old meta row is intact.
			expect((await db.get('meta', 'sync')) as { cursor?: string }).toMatchObject({ cursor: 'c1' });
		} finally {
			db.close();
		}
	});

	it('an OLD build opening a since-upgraded database gets a VersionError (fail-safe degrade)', async () => {
		const U = null;
		const WS = 'ws-downgrade';
		await seedV1Database(U, WS, [row('x', 1)]);
		// Newer build upgrades it to v2.
		const { db } = await openWithMigration(U, WS, 2, 'tombstones');
		db.close();

		// The old build still calls openDB(name, 1) → VersionError, which
		// localIndexPersistence.open() catches and returns null for, degrading
		// that tab to memory-only. Transient and acceptable during a deploy.
		const errName = await versionErrorOnDowngradeOpen(U, WS, 1);
		expect(errName).toBe('VersionError');
	});
});

describe('per-test isolation', () => {
	it('databases do not leak across tests (fresh IDBFactory per test)', async () => {
		// If setup-idb's per-test factory reset regressed, a row written by an
		// earlier test in this file would still be readable here. It must not be.
		expect(await rawItem(null, 'ws-crosstab', 'shared')).toBeUndefined();
		expect(await rawItem(null, 'ws-migrate', 'keep')).toBeUndefined();
	});
});
