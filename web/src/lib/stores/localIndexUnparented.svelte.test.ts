import { afterEach, describe, expect, it } from 'vitest';
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

afterEach(() => localIndex.reset(ws));

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
		);
		expect(localIndex.cursorFor(ws)).toBe('3');
		expect(localIndex.findByIdOrSlug(ws, 'created')?.is_unparented).toBe(true);
	});

	it('bumps the persistent cache contract so version-1 rows are fully resynced', () => {
		expect(LOCAL_INDEX_SCHEMA_VERSION).toBe(2);
	});
});
