import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '$lib/api/client';
import type { Item, ItemChangeRow, ItemIndexRow } from '$lib/types';
import { localIndex } from './localIndex.svelte';
import { LOCAL_INDEX_SCHEMA_VERSION } from './localIndexPersistence';

const ws = 'unparented-projection-test';

function row(id: string, seq: number, isUnparented?: boolean): ItemIndexRow {
	const value = {
		id,
		seq,
		collection_slug: 'tasks',
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
	} as ItemIndexRow;
	if (isUnparented !== undefined) value.is_unparented = isUnparented;
	return value;
}

afterEach(() => {
	vi.restoreAllMocks();
	localIndex.reset(ws);
});

describe('localIndex unparented projection compatibility', () => {
	it('preserves the projection through an optimistic mutation and accepts authoritative metadata at equal seq', () => {
		localIndex.upsert(ws, row('existing', 1, true));
		localIndex.upsert(ws, { ...row('existing', 2), content: 'updated' } as Item);
		expect(localIndex.findByIdOrSlug(ws, 'existing')?.is_unparented).toBe(true);
		expect(localIndex.cursorFor(ws)).toBe('0');

		localIndex.applyDelta(
			ws,
			[{ ...row('existing', 2, false), deleted: false } as ItemChangeRow],
			'2',
			true,
		);
		expect(localIndex.cursorFor(ws)).toBe('2');
		expect(localIndex.findByIdOrSlug(ws, 'existing')?.is_unparented).toBe(false);
	});

	it('fills projection metadata for a newly-created optimistic row at equal seq', () => {
		localIndex.upsert(ws, { ...row('created', 3), content: 'new' } as Item);
		expect(localIndex.findByIdOrSlug(ws, 'created')?.is_unparented).toBeUndefined();
		expect(localIndex.cursorFor(ws)).toBe('0');

		localIndex.applyDelta(
			ws,
			[{ ...row('created', 3, true), deleted: false } as ItemChangeRow],
			'3',
			true,
		);
		expect(localIndex.cursorFor(ws)).toBe('3');
		expect(localIndex.findByIdOrSlug(ws, 'created')?.is_unparented).toBe(true);
	});

	it('bumps the persistent cache contract so version-1 rows are fully resynced', () => {
		expect(LOCAL_INDEX_SCHEMA_VERSION).toBe(3);
	});

	it('fully resyncs when projection permission is downgraded or upgraded', async () => {
		localIndex.upsert(ws, row('scoped', 1, true));
		localIndex.applyDelta(ws, [], '1', true);

		const listIndex = vi.spyOn(api.items, 'listIndex');
		listIndex.mockResolvedValueOnce({
			items: [row('scoped', 1)],
			total: 1,
			cursor: '1',
			includes_unparented_metadata: false,
		});
		expect(await localIndex.ensureProjectionScope(ws, false)).toBe(true);
		expect(localIndex.findByIdOrSlug(ws, 'scoped')?.is_unparented).toBeUndefined();

		listIndex.mockResolvedValueOnce({
			items: [row('scoped', 1, true)],
			total: 1,
			cursor: '1',
			includes_unparented_metadata: true,
		});
		expect(await localIndex.ensureProjectionScope(ws, true)).toBe(true);
		expect(localIndex.findByIdOrSlug(ws, 'scoped')?.is_unparented).toBe(true);
	});

	it('fences a resync-dropped id so a stale optimistic upsert cannot resurrect it', async () => {
		localIndex.upsert(ws, row('visible', 1, true));
		localIndex.upsert(ws, row('hidden', 2, true));
		localIndex.applyDelta(ws, [], '2', true); // cursor=2, scope=unrestricted
		expect(localIndex.findByIdOrSlug(ws, 'hidden')).toBeTruthy();

		// Downgrade resync: the authoritative snapshot omits 'hidden', so it's
		// dropped and fenced.
		vi.spyOn(api.items, 'listIndex').mockResolvedValueOnce({
			items: [row('visible', 1)],
			total: 1,
			cursor: '2',
			includes_unparented_metadata: false,
		});
		expect(await localIndex.ensureProjectionScope(ws, false)).toBe(true);
		expect(localIndex.findByIdOrSlug(ws, 'hidden')).toBeNull();

		// A stale old-scope optimistic create/update response must be refused —
		// it can't resurrect the now-hidden row.
		localIndex.upsert(ws, { ...row('hidden', 3), content: 'stale' } as Item);
		expect(localIndex.findByIdOrSlug(ws, 'hidden')).toBeNull();

		// An authoritative new-scope delta re-adding it lifts the fence...
		localIndex.applyDelta(
			ws,
			[{ ...row('hidden', 3), deleted: false } as ItemChangeRow],
			'3',
			false,
		);
		expect(localIndex.findByIdOrSlug(ws, 'hidden')).toBeTruthy();

		// ...so later optimistic edits are accepted again.
		localIndex.upsert(ws, { ...row('hidden', 4), content: 'fresh' } as Item);
		expect(localIndex.findByIdOrSlug(ws, 'hidden')?.seq).toBe(4);
	});

	it('rejects a stale-epoch brand-new create the fence cannot catch (BUG-2098)', async () => {
		localIndex.upsert(ws, row('visible', 1, true));
		localIndex.applyDelta(ws, [], '1', true); // cursor=1, scope=unrestricted

		// A create is issued under the current (unrestricted) scope. The call
		// site captures the epoch BEFORE awaiting the create response.
		const staleEpoch = localIndex.scopeEpochFor(ws);

		// Before the create resolves, a downgrade resync installs the
		// authoritative snapshot and bumps the scope epoch. The brand-new id is
		// NOT in the snapshot — but it was never in the map either, so it can't
		// be fenced the way a dropped row is; only the epoch guard covers it.
		vi.spyOn(api.items, 'listIndex').mockResolvedValueOnce({
			items: [row('visible', 1)],
			total: 1,
			cursor: '1',
			includes_unparented_metadata: false,
		});
		expect(await localIndex.ensureProjectionScope(ws, false)).toBe(true);
		expect(localIndex.scopeEpochFor(ws)).toBeGreaterThan(staleEpoch);

		// The stale old-scope create response now resolves. Its id was never
		// dropped, so the fence set is empty for it — the epoch guard is the
		// only thing that refuses it.
		localIndex.upsert(ws, { ...row('created', 5), content: 'stale' } as Item, staleEpoch);
		expect(localIndex.findByIdOrSlug(ws, 'created')).toBeNull();

		// A create issued under the CURRENT scope (epoch not behind) is accepted —
		// the guard rejects only responses that predate the resync, not all writes.
		const freshEpoch = localIndex.scopeEpochFor(ws);
		localIndex.upsert(ws, { ...row('created', 6), content: 'fresh' } as Item, freshEpoch);
		expect(localIndex.findByIdOrSlug(ws, 'created')?.seq).toBe(6);
	});
});

// TASK-2099 / PLAN-2095 Phase 2: the collection page's "Unparented only"
// chip gates its visibility off this accessor (not the internal `$state`
// field directly, which isn't exported). Covers the three states a
// consumer must be able to distinguish: unknown, restricted, unrestricted.
describe('localIndex.includesUnparentedMetadataFor', () => {
	it('is null for a workspace that has never resolved a snapshot/delta', () => {
		expect(localIndex.includesUnparentedMetadataFor('never-bootstrapped-ws')).toBeNull();
	});

	it('reflects true after an unrestricted delta and false after a restricted one', () => {
		localIndex.applyDelta(ws, [], '1', true);
		expect(localIndex.includesUnparentedMetadataFor(ws)).toBe(true);

		localIndex.applyDelta(ws, [], '2', false);
		expect(localIndex.includesUnparentedMetadataFor(ws)).toBe(false);
	});
});
