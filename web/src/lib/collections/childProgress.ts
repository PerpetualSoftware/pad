// childProgress: a parent's done/total over children the client already
// holds, for the per-row views that count locally (NestedChildren, ChildChart).
// The parent's own numbers come from the server (GET /progress, BUG-3192).
//
// An ABANDONED child (cancelled, wontfix, …) is left out of BOTH counts
// (BUG-3195, lead ruling): it is not part of the work any more, and counting
// it as done read 100% with nothing delivered. The server applies the same
// rule; this is its client-side copy for rows the server does not count.

import { parseFields, type Item } from '$lib/types';

export interface ChildProgress {
	done: number;
	total: number;
}

/** Children whose status is not abandoned: the ones that count at all. */
export function countedChildren(children: Item[], abandoned: string[]): Item[] {
	return children.filter((c) => !abandoned.includes(parseFields(c).status));
}

export function countChildProgress(children: Item[], terminal: string[], abandoned: string[]): ChildProgress {
	const counted = countedChildren(children, abandoned);
	return {
		total: counted.length,
		done: counted.filter((c) => terminal.includes(parseFields(c).status)).length
	};
}
