import { afterEach, describe, expect, it } from 'vitest';
import type { ItemIndexRow } from '$lib/types';
import { localSearch, parseSearchQuery, withoutArchivedToken } from './localSearch.svelte';

// TASK-2864: `is:archived` widens a search to archived (soft-deleted) rows,
// on the local index and, through include_archived, on the server.

describe('parseSearchQuery is:archived', () => {
	it('sets archived and strips the token from the text', () => {
		expect(parseSearchQuery('is:archived migrate')).toMatchObject({ archived: true, text: 'migrate', body: false });
		expect(parseSearchQuery('migrate IS:ARCHIVED')).toMatchObject({ archived: true, text: 'migrate' });
	});

	it('combines with body:', () => {
		expect(parseSearchQuery('is:archived body:foo')).toMatchObject({ archived: true, body: true, text: 'foo' });
	});

	it('is false without the token, and only the exact token counts', () => {
		expect(parseSearchQuery('migrate').archived).toBe(false);
		expect(parseSearchQuery('is:archivedx migrate')).toMatchObject({ archived: false, text: 'is:archivedx migrate' });
		expect(parseSearchQuery('is: archived')).toMatchObject({ archived: false });
	});
});

describe('withoutArchivedToken', () => {
	it('removes only the archived token', () => {
		expect(withoutArchivedToken('  is:archived  coll:tasks foo ')).toBe('coll:tasks foo');
		expect(withoutArchivedToken('foo')).toBe('foo');
		expect(withoutArchivedToken('is:archived')).toBe('');
	});
});

describe('localSearch.search with is:archived', () => {
	const ws = 'task2864';
	const row = (id: string, n: number, title: string, deleted: boolean): ItemIndexRow =>
		({
			id,
			slug: id,
			title,
			item_number: n,
			collection_slug: 'tasks',
			collection_prefix: 'TASK',
			fields: '{}',
			tags: '[]',
			deleted_at: deleted ? '2026-10-01T00:00:00Z' : undefined,
		}) as unknown as ItemIndexRow;

	afterEach(() => localSearch.reset(ws));

	it('includes archived rows only when the query or the option asks', () => {
		localSearch.rebuild(ws, [row('live', 1, 'pelican live', false), row('gone', 2, 'pelican gone', true)]);
		const ids = (q: string, includeArchived?: boolean) =>
			localSearch.search(ws, q, { includeArchived }).map((r) => r.id).sort();

		expect(ids('pelican')).toEqual(['live']);
		expect(ids('is:archived pelican')).toEqual(['gone', 'live']);
		expect(ids('pelican', true)).toEqual(['gone', 'live']);
		// The exact item-number path honours it too.
		expect(ids('#2')).toEqual([]);
		expect(ids('is:archived #2')).toEqual(['gone']);
	});
});
