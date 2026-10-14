import { describe, it, expect, vi, afterEach } from 'vitest';
import { api } from './client';

// BUG-3704: the server caps one item-list response at 1000 rows, so a page
// that needs a whole collection walks it in pages of 1000 and stops at the
// first short page.
describe('api.items.listAllByCollection (BUG-3704)', () => {
	afterEach(() => vi.unstubAllGlobals());

	function serveRows(total: number) {
		const urls: string[] = [];
		vi.stubGlobal(
			'fetch',
			vi.fn(async (url: string) => {
				urls.push(url);
				const q = new URL(url, 'http://x').searchParams;
				const limit = Number(q.get('limit'));
				const offset = Number(q.get('offset'));
				const n = Math.max(0, Math.min(limit, total - offset));
				const body = Array.from({ length: n }, (_, i) => ({ id: `it-${offset + i}` }));
				return { status: 200, ok: true, json: async () => body };
			})
		);
		return urls;
	}

	it('walks every page and keeps the caller filters', async () => {
		const urls = serveRows(2500);
		const all = await api.items.listAllByCollection('ws', 'tasks', { include_archived: false, limit: 5 });
		expect(all.length).toBe(2500);
		expect(all[2499].id).toBe('it-2499');
		expect(urls.length).toBe(3);
		for (const [i, u] of urls.entries()) {
			const q = new URL(u, 'http://x').searchParams;
			expect(q.get('limit')).toBe('1000');
			expect(q.get('offset')).toBe(String(i * 1000));
			expect(q.get('include_archived')).toBe('false');
		}
	});

	it('asks once more when the last page is exactly full', async () => {
		const urls = serveRows(1000);
		const all = await api.items.listAllByCollection('ws', 'tasks');
		expect(all.length).toBe(1000);
		expect(urls.length).toBe(2);
	});
});
