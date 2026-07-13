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
});
