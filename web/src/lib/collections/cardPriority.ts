// Setting an item's priority from its card (TASK-2214, audit C32).
//
// The card's priority chip was display-only, and the only other way to set a
// priority from a list or board was a lane bulk-set. The chip is now the same
// picker the status chip is (BUG-3157). The write comes from the host page
// through Svelte context rather than a prop threaded through every view that
// renders cards (List, Board, Table children, Starred, Tags, Roles): only the
// collection page provides it, and a card with no provider shows the static
// chip as before.
import { getContext, setContext } from 'svelte';
import type { Item } from '$lib/types';

/** Exported for tests, which provide the writer through render's `context`. */
export const CARD_PRIORITY_KEY = Symbol('card-priority-writer');
const KEY = CARD_PRIORITY_KEY;

export type CardPriorityWriter = (item: Item, priority: string) => void;

export function provideCardPriorityWriter(write: CardPriorityWriter): void {
	setContext(KEY, write);
}

export function cardPriorityWriter(): CardPriorityWriter | undefined {
	return getContext<CardPriorityWriter | undefined>(KEY);
}
