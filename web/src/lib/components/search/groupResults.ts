import type { Collection, SearchResult } from '$lib/types';

export interface ResultGroup {
	icon: string;
	name: string;
	results: SearchResult[];
}

/**
 * Group search results by collection slug, in first-seen order, for the
 * command palette.
 *
 * NULL-PROTOTYPE (BUG-3054). The key is a collection slug, which a user picks:
 * a collection named "Constructor" has the slug `constructor`, and on a plain
 * object `!groups['constructor']` found Object's constructor, skipped creating
 * the group, and the push onto its `.results` threw, taking the whole palette
 * down on any search that matched an item there. The palette reads the result
 * only through `Object.entries`, which a null-prototype object answers the same.
 */
export function groupResultsByCollection(
	results: SearchResult[],
	collections: Pick<Collection, 'slug' | 'icon' | 'name'>[],
): Record<string, ResultGroup> {
	const groups: Record<string, ResultGroup> = Object.create(null);
	for (const r of results) {
		const slug = r.item.collection_slug || 'unknown';
		if (!groups[slug]) {
			const coll = collections.find((c) => c.slug === slug);
			groups[slug] = {
				icon: r.item.collection_icon || coll?.icon || '📦',
				name: coll?.name || slug,
				results: [],
			};
		}
		groups[slug].results.push(r);
	}
	return groups;
}

/**
 * The grouped results in the order the palette RENDERS them (TASK-2234):
 * group by group, each in first-seen order. Keyboard navigation and
 * aria-activedescendant walk this order, so the selection moves down the list
 * the user sees rather than jumping between groups in rank order.
 */
export function inRenderedOrder(groups: Record<string, ResultGroup>): SearchResult[] {
	return Object.values(groups).flatMap((g) => g.results);
}

/**
 * Where the selection belongs after a page of results is appended
 * (TASK-2234, codex r2). Grouped, an appended row can land ABOVE the
 * selected one (A1, B1 + A2 renders A1, A2, B1), so the same index would
 * name a different item; the selection follows the item, by id.
 */
export function reselectAfterAppend(
	before: SearchResult[],
	after: SearchResult[],
	idx: number,
): number {
	const selected = idx >= 0 ? before[idx] : undefined;
	if (!selected) return idx < 0 ? -1 : idx;
	return after.findIndex((r) => r.item.id === selected.item.id);
}

/**
 * What the palette's live region says (TASK-2234): the count of what a
 * search found, once every search it started has answered.
 */
export function resultAnnouncement(
	query: string,
	loading: boolean,
	contentLoading: boolean,
	count: number,
): string {
	if (!query.trim() || loading || contentLoading) return '';
	return `${count} result${count === 1 ? '' : 's'}`;
}
