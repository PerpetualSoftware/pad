import type { Collection, Item, SearchResult } from '$lib/types';
import {
	doneFieldKey,
	doneFieldTerminalOptions,
	getAbandonedOptions,
	getFieldValue,
} from '$lib/types';

/**
 * A row the palette lists. A server result says where it matched; a row built
 * from the local index does not (that index covers titles, refs, tags and
 * field values and reports only a score), so it carries no `matched_in`
 * rather than a guessed one.
 */
export type PaletteResult = Omit<SearchResult, 'matched_in'> & {
	matched_in?: SearchResult['matched_in'];
};

/**
 * Label for a result row's workspace pill (TASK-4805).
 * Returns null when `r.workspace` is absent; otherwise, when `allMode` is true
 * or the result belongs to a different workspace than `currentSlug`, returns
 * the workspace name with slug fallback; else null.
 */
export function workspaceLabel(
	r: { workspace?: { slug: string; name?: string } },
	currentSlug: string | undefined,
	allMode: boolean,
): string | null {
	if (!r.workspace) return null;
	if (allMode || r.workspace.slug !== currentSlug) {
		return r.workspace.name || r.workspace.slug;
	}
	return null;
}

/**
 * Label for a workspace filter chip: the workspace name from the first result
 * carrying that slug, else the slug itself.
 */
export function workspaceChipLabel(
	slug: string,
	results: { workspace?: { slug: string; name?: string } }[],
): string {
	const match = results.find((r) => r.workspace?.slug === slug && r.workspace.name);
	return match?.workspace?.name || slug;
}

/**
 * Label for a collection filter chip: the `collection_name` of the first result
 * whose `collection_slug === slug`, else the slug. Avoids reading collection
 * names from the current workspace's store when searching across workspaces.
 */
export function collectionChipLabel(
	slug: string,
	results: { item: { collection_slug?: string; collection_name?: string } }[],
): string {
	const match = results.find((r) => r.item.collection_slug === slug && r.item.collection_name);
	return match?.item.collection_name || slug;
}

/**
 * Relevance multiplier for local (in-RAM) search results (TASK-4805):
 * 0.35 when the item's done-field value is one of its collection's abandoned
 * options, 0.6 when it is one of the terminal options, else 1.
 */
export function localStatusFactor(
	item: Pick<Item, 'collection_slug' | 'fields'>,
	collections: Collection[],
): number {
	const coll = collections.find((c) => c.slug === item.collection_slug);
	if (!coll) return 1;
	const key = doneFieldKey(coll);
	const raw = getFieldValue(item as Item, key);
	if (typeof raw !== 'string') return 1;
	const val = raw.trim().toLowerCase();
	if (!val) return 1;
	if (getAbandonedOptions(coll).some((v) => v.trim().toLowerCase() === val)) {
		return 0.35;
	}
	if (doneFieldTerminalOptions(coll).some((v) => v.trim().toLowerCase() === val)) {
		return 0.6;
	}
	return 1;
}

/**
 * Sort local (in-RAM) search results by relevance (rank desc, then updated_at
 * desc, then id asc) or by recency (updated_at desc, then id asc).
 */
export function sortLocalResults<T extends { rank: number; item: { id: string; updated_at?: string } }>(
	rows: T[],
	sortMode: 'relevance' | 'updated_at',
): T[] {
	return [...rows].sort((a, b) => {
		if (sortMode === 'relevance' && b.rank !== a.rank) {
			return b.rank - a.rank;
		}
		const aU = a.item.updated_at ?? '';
		const bU = b.item.updated_at ?? '';
		if (aU !== bU) return aU < bU ? 1 : -1;
		return a.item.id < b.item.id ? -1 : a.item.id > b.item.id ? 1 : 0;
	});
}

/**
 * Where the selection belongs after a page of results is appended
 * (TASK-2234): the selection follows the item, by id.
 */
export function reselectAfterAppend(
	before: PaletteResult[],
	after: PaletteResult[],
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

/**
 * The source label shown beside a result's snippet: "in comment", "in note"
 * or "in decision" when the snippet was cut from a comment, an implementation
 * note or a decision-log entry (TASK-4491), the wording the CLI uses. Every
 * other source reads as the item's own text and gets no label.
 */
export function matchMarker(result: Pick<PaletteResult, 'matched_in'>): string {
	switch (result.matched_in) {
		case 'comment':
		case 'note':
		case 'decision':
			return `in ${result.matched_in}`;
		default:
			return '';
	}
}
