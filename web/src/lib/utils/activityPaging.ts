/**
 * Keyset paging for the activity feeds (BUG-2781).
 *
 * The feeds used to page by `offset = rows already held`. Activity rows MOVE:
 * a debounce merge restamps an older row to now, every row behind it shifts
 * down one position, and the next offset page repeats a row the client
 * already has. Every feed renders with a keyed `{#each ... (row.id)}`, where a
 * repeated key is a Svelte runtime error rather than a visible duplicate.
 *
 * The server now accepts `before` + `before_id`: rows strictly after that row
 * in (created_at, id) descending order. The client takes both from the last
 * row it holds, so a row moving ahead of the cursor cannot shift what follows.
 *
 * NOT FIXED by any paging scheme: the moved row itself is now ahead of the
 * cursor, so no later page returns it; it shows only when the head of the
 * feed is re-read.
 */

export interface ActivityCursor {
	before: string;
	before_id: string;
}

interface PagedRow {
	id: string;
	created_at: string;
}

/** The cursor that continues after `rows`, or null when there are none. */
export function cursorAfter(rows: readonly PagedRow[]): ActivityCursor | null {
	const last = rows[rows.length - 1];
	if (!last) return null;
	return { before: last.created_at, before_id: last.id };
}

/**
 * Appends `page` to `held`, dropping any row whose id is already held. The
 * keyset removes the repeats offset paging produced; this is the second guard,
 * because a repeated id breaks a keyed each block outright.
 */
export function appendUnique<T extends { id: string }>(held: readonly T[], page: readonly T[]): T[] {
	const seen = new Set(held.map((r) => r.id));
	const out = [...held];
	for (const row of page) {
		if (seen.has(row.id)) continue;
		seen.add(row.id);
		out.push(row);
	}
	return out;
}

/**
 * Folds a fresh read of the feed's HEAD into the rows held (BUG-3160). This is
 * what makes a row the debounce merge restamped visible: it is in `fresh`
 * under the same id, at its new position and with its new content.
 *
 * - A row in `fresh` replaces the held row with the same id.
 * - A held row ABSENT from `fresh` is kept: the head is only the newest page,
 *   and a row that rolled off it is not a deleted row (the rule ItemTimeline's
 *   refresh follows, BUG-2773).
 * - The result is in the feed's order, (created_at, id) descending, so a moved
 *   row lands at the top rather than where it was.
 */
export function mergeHead<T extends PagedRow>(held: readonly T[], fresh: readonly T[]): T[] {
	const byId = new Map<string, T>();
	for (const row of held) byId.set(row.id, row);
	for (const row of fresh) byId.set(row.id, row);
	return [...byId.values()].sort((a, b) =>
		a.created_at === b.created_at ? (a.id < b.id ? 1 : a.id > b.id ? -1 : 0) : a.created_at < b.created_at ? 1 : -1,
	);
}
