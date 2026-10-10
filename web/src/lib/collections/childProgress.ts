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
//
// So is every child of a REFERENCE collection (PLAN-3535, `tracks_work:
// false`), whatever its status: a doc under a plan is not part of its work.
// The parent page lists those children in their own References group. A
// child's own state (childState) is unchanged by it: a reference doc can still
// be draft or published, and lists that filter on open items keep using it.

import {
	collectionTracksWork,
	DEFAULT_ABANDONED_STATUSES,
	doneFieldKey,
	doneFieldTerminalOptions,
	getAbandonedOptions,
	isTerminalStatusDefault,
	parseFields,
	type Collection,
	type Item
} from '$lib/types';
import { safeText } from '$lib/fields/fieldShape';

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

/** The state a child's ROW shows (done styling), judged by its own collection
 * out of `collections`. Same rule as the counts, except that a done-field value
 * stored in the wrong shape (an array, an object; BUG-3052) is read through
 * safeText first, as the row's status lane is, so a row and its lane agree.
 * The counts keep the strict reading, which is the server's. */
export function childRowState(child: Item, collections: Collection[]): ChildState {
	const collection = collectionOf(child, collections);
	const fields = parseFields(child);
	const key = collection ? doneFieldKey(collection) : 'status';
	const value = fields[key];
	if (value == null || typeof value === 'string') return childState(child, collection);
	return childState({ ...child, fields: JSON.stringify({ ...fields, [key]: safeText(value) }) }, collection);
}

/** How items of `collection` holding `status` count: the same rule as
 * childState, for callers that hold a status tally rather than items (the
 * dashboard's per-collection summary, which tallies the `status` field). When
 * the collection's done field is not `status`, the tally cannot say, and the
 * default lists judge it, as for a child with no collection. */
export function statusState(collection: Collection, status: string): ChildState {
	const item = { fields: JSON.stringify({ status }) } as Item;
	return childState(item, doneFieldKey(collection) === 'status' ? collection : undefined);
}

/** Whether a child belongs to a REFERENCE collection (PLAN-3535). A child
 * whose collection is not known counts as work, as the server does. */
export function isReferenceChild(child: Item, collections: Collection[]): boolean {
	const collection = collectionOf(child, collections);
	return !!collection && !collectionTracksWork(collection);
}

/** Splits children into the work that progress counts and the reference
 * material the parent page lists apart (PLAN-3535). Order is kept. */
export function splitReferenceChildren(
	children: Item[],
	collections: Collection[]
): { work: Item[]; reference: Item[] } {
	const work: Item[] = [];
	const reference: Item[] = [];
	for (const c of children) (isReferenceChild(c, collections) ? reference : work).push(c);
	return { work, reference };
}

/** Children that count at all: every work child that is not abandoned. */
export function countedChildren(children: Item[], collections: Collection[]): Item[] {
	return children.filter(
		(c) => !isReferenceChild(c, collections) && childState(c, collectionOf(c, collections)) !== 'out'
	);
}

/** Whether a counted child is done, by its own collection. */
export function isChildDone(child: Item, collections: Collection[]): boolean {
	return childState(child, collectionOf(child, collections)) === 'done';
}

export function countChildProgress(children: Item[], collections: Collection[]): ChildProgress {
	let done = 0;
	let total = 0;
	for (const c of children) {
		if (isReferenceChild(c, collections)) continue;
		const state = childState(c, collectionOf(c, collections));
		if (state === 'out') continue;
		total++;
		if (state === 'done') done++;
	}
	return { done, total };
}
