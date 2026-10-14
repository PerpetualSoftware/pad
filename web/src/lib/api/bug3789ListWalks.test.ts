import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from './client';
import { walkOffsetPages } from './listWalk';
import { walkSharePages, type SharePage } from './shareWalk';

// BUG-3789 / BUG-3790 / BUG-3792: the children list, the starred list and a
// shared collection are bounded per page, so the client walks every page.

const row = (id: string) => ({ id });
const coll = (items: string[], more: boolean, next?: number, refs?: Record<string, unknown>): SharePage =>
	({ type: 'collection', collection: { name: 'C' }, items: items.map((ref) => ({ ref, title: ref })), has_more: more, next_offset: next, attachment_refs: refs }) as unknown as SharePage;

describe('walkOffsetPages', () => {
	it('asks page after page until a short page, de-duplicating by id', async () => {
		const asked: [number, number][] = [];
		const pages = [[row('a'), row('b')], [row('b'), row('c')], [row('d')]];
		const got = await walkOffsetPages(async (limit, offset) => {
			asked.push([limit, offset]);
			return pages[asked.length - 1];
		}, 2);
		expect(asked).toEqual([[2, 0], [2, 2], [2, 4]]);
		expect(got.map((r) => r.id)).toEqual(['a', 'b', 'c', 'd']);
	});

	it('an exact multiple costs one empty page', async () => {
		const pages = [[row('a'), row('b')], []];
		let n = 0;
		const got = await walkOffsetPages(async () => pages[n++], 2);
		expect(n).toBe(2);
		expect(got).toHaveLength(2);
	});

	it('fails past the page bound', async () => {
		let n = 0;
		await expect(walkOffsetPages(async () => [row(`r${n++}`), row(`s${n++}`)], 2, 3)).rejects.toThrow('3 pages');
	});
});

describe('walkSharePages', () => {
	it('follows next_offset and merges items and attachment refs', async () => {
		const asked: number[] = [];
		const pages = [coll(['A', 'B'], true, 2, { x: { mime_type: 'image/png', filename: 'x' } }), coll(['C'], false, 3, { y: { mime_type: 'image/png', filename: 'y' } })];
		const got = (await walkSharePages(async (o) => {
			asked.push(o);
			return pages[asked.length - 1];
		})) as SharePage;
		expect(asked).toEqual([0, 2]);
		expect(got.items?.map((i) => i.ref)).toEqual(['A', 'B', 'C']);
		expect(Object.keys(got.attachment_refs ?? {})).toEqual(['x', 'y']);
		expect(got.has_more).toBe(false);
		expect(got.next_offset).toBeUndefined();
	});

	it('an item share, a refusal or an older server is one page', async () => {
		for (const first of [{ type: 'item', item: { title: 't' } }, { require_password: true }, { type: 'collection', items: [] }] as SharePage[]) {
			let n = 0;
			expect(await walkSharePages(async () => (n++, first))).toBe(first);
			expect(n).toBe(1);
		}
	});

	it('fails past the bound, and on has_more without a usable next_offset', async () => {
		await expect(walkSharePages(async (o) => coll([`I${o}`], true, o + 1), 3)).rejects.toThrow('3 pages');
		await expect(walkSharePages(async () => coll(['A'], true, undefined))).rejects.toThrow();
	});
});

// CONVE-19: the API methods the pages call are the walks.
describe('API wiring (BUG-3789 / BUG-3790 / BUG-3792)', () => {
	afterEach(() => vi.unstubAllGlobals());

	function stub(bodies: unknown[]) {
		const urls: string[] = [];
		vi.stubGlobal(
			'fetch',
			vi.fn(async (url: string) => {
				urls.push(String(url));
				return new Response(JSON.stringify(bodies[urls.length - 1]), { status: 200, headers: { 'Content-Type': 'application/json' } });
			})
		);
		return urls;
	}
	const full = (p: string) => Array.from({ length: 1000 }, (_, i) => row(`${p}${i}`));
	const tail = (u: string) => u.slice(u.indexOf('/workspaces/') >= 0 ? u.indexOf('/workspaces/') : u.indexOf('/s/'));

	it('children walks by offset', async () => {
		const urls = stub([full('c'), [row('last')]]);
		const got = await api.items.children('ws', 'p');
		expect(got).toHaveLength(1001);
		expect(urls.map(tail)).toEqual(['/workspaces/ws/items/p/children?limit=1000&offset=0', '/workspaces/ws/items/p/children?limit=1000&offset=1000']);
	});

	it('starred walks by offset and keeps its params', async () => {
		const urls = stub([full('s'), []]);
		await api.items.starred('ws', { include_terminal: true, summary: true });
		expect(urls.map(tail)).toEqual([
			'/workspaces/ws/starred?include_terminal=true&summary=true&limit=1000&offset=0',
			'/workspaces/ws/starred?include_terminal=true&summary=true&limit=1000&offset=1000'
		]);
	});

	it('share.get walks a shared collection', async () => {
		const urls = stub([coll(['A'], true, 1000), coll(['B'], false, 1001)]);
		const got = await api.share.get('tok');
		expect(got.items?.map((i) => i.ref)).toEqual(['A', 'B']);
		expect(urls.map(tail)).toEqual(['/s/tok', '/s/tok?offset=1000']);
	});
});
