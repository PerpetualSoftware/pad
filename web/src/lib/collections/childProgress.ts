// childProgress: a parent's done/total over children the client already
// holds, for the per-row views that count locally (NestedChildren, ChildChart).
// The parent's own numbers come from the server (GET /progress, BUG-3192).
//
// Each child is judged by ITS OWN collection: that collection's done field,
// terminal values and abandoned values, compared case-insensitively. That is
// the server's rule (server childProgressState / store
// buildChildrenAbandonedExpr), so a nested count and /progress agree about the
// same child.
//
// An ABANDONED child (cancelled, wontfix, …) is left out of BOTH counts
// (BUG-3195, lead ruling): it is not part of the work any more, and counting
// it as done read 100% with nothing delivered.

import {
	DEFAULT_ABANDONED_STATUSES,
	doneFieldKey,
	doneFieldTerminalOptions,
	getAbandonedOptions,
	isTerminalStatusDefault,
	parseFields,
	type Collection,
	type Item
} from '$lib/types';

export type ChildState = 'out' | 'done' | 'open';

export interface ChildProgress {
	done: number;
	total: number;
}

const lowerIn = (values: string[], v: unknown) =>
	typeof v === 'string' && values.some((x) => x.toLowerCase() === v.toLowerCase());

/** How one child counts toward its parent's progress. With no collection to
 * judge by, `status` against the default lists, as the server falls back. */
export function childState(child: Item, collection: Collection | undefined): ChildState {
	const fields = parseFields(child);
	if (!collection) {
		const status = fields.status;
		if (lowerIn(DEFAULT_ABANDONED_STATUSES, status)) return 'out';
		return typeof status === 'string' && isTerminalStatusDefault(status.toLowerCase()) ? 'done' : 'open';
	}
	const value = fields[doneFieldKey(collection)];
	if (lowerIn(getAbandonedOptions(collection), value)) return 'out';
	return lowerIn(doneFieldTerminalOptions(collection), value) ? 'done' : 'open';
}

function collectionOf(child: Item, collections: Collection[]): Collection | undefined {
	return collections.find((c) => c.slug === child.collection_slug);
}

/** Children that count at all: every child that is not abandoned. */
export function countedChildren(children: Item[], collections: Collection[]): Item[] {
	return children.filter((c) => childState(c, collectionOf(c, collections)) !== 'out');
}

/** Whether a counted child is done, by its own collection. */
export function isChildDone(child: Item, collections: Collection[]): boolean {
	return childState(child, collectionOf(child, collections)) === 'done';
}

export function countChildProgress(children: Item[], collections: Collection[]): ChildProgress {
	let done = 0;
	let total = 0;
	for (const c of children) {
		const state = childState(c, collectionOf(c, collections));
		if (state === 'out') continue;
		total++;
		if (state === 'done') done++;
	}
	return { done, total };
}
