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
