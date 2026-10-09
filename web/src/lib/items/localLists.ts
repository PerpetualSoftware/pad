// Lists the tag and starred pages build from the local index (TASK-2231),
// instead of fetching full-content items to render card summaries. Each rule
// here mirrors the server query the page used to call, so the two answers
// agree; where they cannot, the comment says so.

import { childState } from '$lib/collections/childProgress';
import type { Collection, Item } from '$lib/types';

/** The item's tags exactly as stored. NOT `parseTags`, which folds case
 * variants into one: the tags index counts "UX" and "ux" separately
 * (ListWorkspaceTags groups by the exact value), and this page must list the
 * items that count says it holds. */
function storedTags(item: Pick<Item, 'tags'>): string[] {
	if (!item.tags) return [];
	try {
		const parsed: unknown = JSON.parse(item.tags);
		return Array.isArray(parsed) ? parsed.filter((t): t is string => typeof t === 'string') : [];
	} catch {
		return [];
	}
}

/** Whether the item carries `tag`, compared exactly: Postgres's
 * `tags @> '["tag"]'`. SQLite's `LIKE '%"tag"%'` folded ASCII case and read
 * `_` and `%` in a tag as wildcards, so on SQLite the server could list items
 * the tags index did not count; this follows the index. */
export function hasTag(item: Pick<Item, 'tags'>, tag: string): boolean {
	return storedTags(item).includes(tag);
}

/** Open, judged by the item's own collection: its done field and terminal
 * values, case-insensitively, as the server's non-terminal filters do
 * (ListItems' nonTerminalFilter, ListStarredItems' isTerminalWithContext).
 * An item whose collection the caller cannot list (an item-level grant) is
 * judged by `status` against the default list, which the server only does
 * when it has no schema; that is the one place the two can differ. */
export function isOpen(item: Item, collections: Collection[]): boolean {
	return childState(item, collections.find((c) => c.id === item.collection_id)) === 'open';
}

/** The server list's default order: pinned first, then most recently
 * updated (buildItemSort with no sort), with the id as the last tiebreak so
 * the order is stable. */
export function byPinnedThenRecent(a: Item, b: Item): number {
	if (!!a.pinned !== !!b.pinned) return a.pinned ? -1 : 1;
	if (a.updated_at !== b.updated_at) return a.updated_at < b.updated_at ? 1 : -1;
	if (a.id === b.id) return 0;
	return a.id < b.id ? -1 : 1;
}
