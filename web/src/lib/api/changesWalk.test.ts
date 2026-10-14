import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from './client';
import type { ChangesResponse, Item } from '$lib/types';
import { walkChanges, type ChangesPageQuery } from './changesWalk';

// BUG-3791: /changes is paged; the client walks every page before it reports
// a delta, and the merged delta is what a single unbounded response would be.

function item(id: string, title: string, deleted = false): Item {
	return { id, title, deleted_at: deleted ? '2026-10-14T00:00:00Z' : null } as Item;
}

function page(updated: Item[], more: string | null, serverTime = 100): ChangesResponse {
	return {
		updated,
		deleted: updated.filter((i) => i.deleted_at).map((i) => i.id),
		server_time: serverTime,
		collections_changed: updated.length > 0,
		has_more: more !== null,
		...(more !== null ? { next_cursor: more } : {})
	};
}

function server(pages: ChangesResponse[]) {
	const asked: ChangesPageQuery[] = [];
	return {
		asked,
		fetch: async (q: ChangesPageQuery) => {
			asked.push(q);
			return pages[asked.length - 1];
		}
	};
}

describe('walkChanges (BUG-3791)', () => {
	it('a single page is returned as is', async () => {
		const only = page([item('a', 'A')], null);
		const s = server([only]);
		expect(await walkChanges(s.fetch, 5)).toBe(only);
		expect(s.asked).toEqual([{ since: 5 }]);
	});

	it('an old server without has_more is one page', async () => {
		const old = { updated: [item('a', 'A')], deleted: [], server_time: 7, collections_changed: true } as unknown as ChangesResponse;
		const s = server([old]);
		expect(await walkChanges(s.fetch, 5)).toBe(old);
		expect(s.asked).toHaveLength(1);
	});

	it('follows next_cursor to the end and keeps each item\'s last row', async () => {
		const s = server([
			page([item('a', 'A1'), item('b', 'B', true)], 'c1'),
			page([item('c', 'C'), item('a', 'A2')], 'c2'),
			page([item('b', 'B restored')], null)
		]);
		const got = await walkChanges(s.fetch, 5);
		expect(s.asked).toEqual([{ since: 5 }, { cursor: 'c1' }, { cursor: 'c2' }]);
		expect(got.updated.map((i) => [i.id, i.title])).toEqual([
			['c', 'C'],
			['a', 'A2'],
			['b', 'B restored']
		]);
		// Deleted in page 1, restored in page 3: not deleted.
		expect(got.deleted).toEqual([]);
		expect(got.server_time).toBe(100);
		expect(got.has_more).toBe(false);
	});

	it('a row deleted in a later page is reported deleted', async () => {
		const s = server([page([item('a', 'A')], 'c1'), page([item('a', 'A', true)], null)]);
		expect((await walkChanges(s.fetch, 5)).deleted).toEqual(['a']);
	});

	it('a failed page fails the walk, so no partial delta reaches the cursor', async () => {
		const pages = [page([item('a', 'A')], 'c1')];
		let n = 0;
		const fetch = async () => {
			if (n++ === 0) return pages[0];
			throw new Error('down');
		};
		await expect(walkChanges(fetch, 5)).rejects.toThrow('down');
	});

	it('gives up past the page bound, and a has_more without a cursor', async () => {
		const endless = async (q: ChangesPageQuery) =>
			page([item('cursor' in q ? q.cursor : 'first', 'x')], 'next' + Math.random());
		await expect(walkChanges(endless, 5, 3)).rejects.toThrow('3 pages');
		const broken = server([{ ...page([item('a', 'A')], null), has_more: true }]);
		await expect(walkChanges(broken.fetch, 5)).rejects.toThrow();
	});
});

// CONVE-19: the walk is what the sync service's `api.changes.since` runs.
describe('api.changes.since walks the pages (BUG-3791 wiring)', () => {
	afterEach(() => {
		vi.unstubAllGlobals();
	});

	it('asks with since, then with each cursor, and returns the merge', async () => {
		const urls: string[] = [];
		const bodies = [page([item('a', 'A')], 'c1.x'), page([item('b', 'B')], null)];
		vi.stubGlobal(
			'fetch',
			vi.fn(async (url: string) => {
				urls.push(String(url));
				return new Response(JSON.stringify(bodies[urls.length - 1]), {
					status: 200,
					headers: { 'Content-Type': 'application/json' }
				});
			})
		);
		const got = await api.changes.since('ws', 42);
		expect(urls.map((u) => u.slice(u.indexOf('/workspaces/')))).toEqual([
			'/workspaces/ws/changes?since=42',
			'/workspaces/ws/changes?cursor=c1.x'
		]);
		expect(got.updated.map((i) => i.id)).toEqual(['a', 'b']);
	});
});
