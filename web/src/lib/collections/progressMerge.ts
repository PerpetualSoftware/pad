// progressMerge — the collection-page progress fetch + merge, extracted from
// [collection]/+page.svelte as part of TASK-2029. That page fetched and merged
// child-item + markdown-checkbox progress into per-item badge data in TWO
// places (the `refreshProgress` helper and inline in `loadCollection`) with
// byte-identical merge logic. This is the single shared implementation both
// call sites now use.
//
// The merge functions are pure (no IO, no Svelte) and unit-tested;
// `fetchCollectionProgress` wraps them with the parallel API fetch. Each fetch
// swallows its own error into empty rows, matching the call sites' existing
// `.catch(() => [])` behaviour.

import { api } from '$lib/api/client';

export interface ProgressRow {
	item_id: string;
	total: number;
	done: number;
}

export interface ProgressEntry {
	total: number;
	done: number;
	label?: string;
}

export type ProgressMap = Record<string, ProgressEntry>;

/** Build the badge map for a `plans` collection from plans-progress rows. */
export function plansProgressToMap(rows: ProgressRow[]): ProgressMap {
	const map: ProgressMap = {};
	for (const p of rows) {
		map[p.item_id] = { total: p.total, done: p.done };
	}
	return map;
}

/**
 * Merge child-item progress with markdown-checkbox progress into the per-item
 * badge map (BUG-1509). Preference order per item:
 *   1. real linked children (total > 0)          → label "tasks"
 *   2. else markdown checkboxes, if any           → label "done"
 * child-progress returns ALL items (total=0 for those with no linked
 * children), so it drives the has-children decision; the final loop is a
 * defensive catch for any item present only in checkbox-progress.
 */
export function mergeChildAndCheckboxProgress(
	childRows: ProgressRow[],
	checkboxRows: ProgressRow[],
): ProgressMap {
	const checkboxMap: Record<string, { total: number; done: number }> = {};
	for (const p of checkboxRows) {
		checkboxMap[p.item_id] = { total: p.total, done: p.done };
	}

	const map: ProgressMap = {};
	for (const p of childRows) {
		if (p.total > 0) {
			map[p.item_id] = { total: p.total, done: p.done, label: 'tasks' };
		} else if (checkboxMap[p.item_id]) {
			map[p.item_id] = { ...checkboxMap[p.item_id], label: 'done' };
		}
	}
	// Defensive: cover any items only in checkbox-progress (shouldn't happen
	// since child-progress covers all items, but be safe).
	for (const p of checkboxRows) {
		if (!map[p.item_id]) {
			map[p.item_id] = { total: p.total, done: p.done, label: 'done' };
		}
	}
	return map;
}

/**
 * Fetch child + checkbox progress for a non-plans collection in parallel and
 * merge them into the badge map. Each fetch resolves to `[]` on error so this
 * never rejects — mirroring the call sites' pre-extraction `.catch(() => [])`.
 */
export async function fetchCollectionProgress(
	ws: string,
	coll: string,
	opts: { includeArchived: boolean },
): Promise<ProgressMap> {
	const [childRows, checkboxRows] = await Promise.all([
		api.items
			.collectionChildProgress(ws, coll, { includeArchived: opts.includeArchived })
			.catch(() => [] as ProgressRow[]),
		api.items
			.collectionCheckboxProgress(ws, coll, { includeArchived: opts.includeArchived })
			.catch(() => [] as ProgressRow[]),
	]);
	return mergeChildAndCheckboxProgress(childRows, checkboxRows);
}
