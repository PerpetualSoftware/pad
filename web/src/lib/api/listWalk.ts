/** The server's page ceiling for bounded lists (BUG-3704, BUG-3789). */
export const LIST_PAGE_SIZE = 1000;

/** The most pages one list walk follows before failing (50,000 rows). */
export const MAX_LIST_PAGES = 50;

/**
 * Pull every page of a bounded bare-array list by offset (BUG-3789, BUG-3790).
 * The body carries no paging state, and `request` does not expose the
 * has-more headers, so a page shorter than the size asked for is the last;
 * a list of exactly a multiple of the size costs one empty request. Rows are
 * de-duplicated by id, first occurrence kept, since a row that moves between
 * requests can come back in a later page.
 */
export async function walkOffsetPages<T extends { id: string }>(
	fetchPage: (limit: number, offset: number) => Promise<T[]>,
	pageSize = LIST_PAGE_SIZE,
	maxPages = MAX_LIST_PAGES
): Promise<T[]> {
	const out: T[] = [];
	const seen = new Set<string>();
	for (let page = 0; page < maxPages; page++) {
		const rows = await fetchPage(pageSize, page * pageSize);
		for (const row of rows) {
			if (seen.has(row.id)) continue;
			seen.add(row.id);
			out.push(row);
		}
		if (rows.length < pageSize) return out;
	}
	throw new Error(`list did not finish within ${maxPages} pages`);
}
