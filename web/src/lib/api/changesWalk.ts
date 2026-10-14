import type { ChangesResponse } from '$lib/types';

/**
 * The most pages one /changes walk follows before giving up (BUG-3791). At the
 * server's 1000-row page that is 50,000 changes; past it the walk throws, and
 * every caller already answers a failed /changes with a full refresh.
 */
export const MAX_CHANGES_PAGES = 50;

export type ChangesPageQuery = { since: number } | { cursor: string };

/**
 * Pull every page of one /changes walk and merge them into a single response
 * (BUG-3791). The server bounds each page and continues by a cursor on its
 * write order, so a row written during the walk can come back in a later page
 * too. The merge keeps each item's LAST row, at its last position, and derives
 * `deleted` from those rows, so an item deleted in one page and restored in a
 * later one is not reported deleted. Every page repeats the first page's
 * `server_time`; the merged response carries it, and only once the walk is
 * complete, so a caller's cursor never moves past changes it has not received.
 */
export async function walkChanges(
	fetchPage: (q: ChangesPageQuery) => Promise<ChangesResponse>,
	sinceMs: number,
	maxPages = MAX_CHANGES_PAGES
): Promise<ChangesResponse> {
	let page = await fetchPage({ since: sinceMs });
	if (!page.has_more) return page;
	const serverTime = page.server_time;
	const pages = [page];
	while (page.has_more) {
		if (pages.length >= maxPages || !page.next_cursor) {
			throw new Error(`/changes walk did not finish within ${maxPages} pages`);
		}
		page = await fetchPage({ cursor: page.next_cursor });
		pages.push(page);
	}
	const latest = new Map<string, ChangesResponse['updated'][number]>();
	for (const p of pages) {
		for (const item of p.updated) {
			latest.delete(item.id);
			latest.set(item.id, item);
		}
	}
	const updated = [...latest.values()];
	return {
		updated,
		deleted: updated.filter((item) => item.deleted_at).map((item) => item.id),
		server_time: serverTime,
		collections_changed: pages.some((p) => p.collections_changed),
		has_more: false
	};
}
