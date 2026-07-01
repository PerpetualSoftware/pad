// Menu-driven manual reordering (IDEA-1898).
//
// The companion to drag-to-reorder: a per-item context menu offers
// "move to top / bottom / up / down" so an item can be repositioned
// without a drag — essential on touch (board drag is disabled on
// mobile) and in long lists where dragging across many rows is painful.
//
// Both surfaces persist the same `sort_order` field. This helper takes a
// group of items in their CURRENT displayed order and returns the
// `sort_order` updates needed to move one item in a direction. It always
// produces a dense 0..n-1 reindex of the group, so it is collision-safe
// even when siblings currently share a `sort_order` (a naive neighbour
// swap would be a visual no-op in that case). Callers persist only the
// rows that actually changed — the returned list is already filtered to
// those.
import type { Item } from '$lib/types';

export type ReorderDirection = 'top' | 'bottom' | 'up' | 'down';

export interface ReorderUpdate {
	item: Item;
	sort_order: number;
}

/**
 * Return `ordered` with `itemId` moved one step or to an end per `dir`.
 * Returns the SAME array reference when the move is a no-op (item already
 * at the target edge, or not found) so callers can cheaply detect it.
 */
export function reorderedList(
	ordered: Item[],
	itemId: string,
	dir: ReorderDirection
): Item[] {
	const from = ordered.findIndex((i) => i.id === itemId);
	if (from === -1) return ordered;

	let to: number;
	switch (dir) {
		case 'top':
			to = 0;
			break;
		case 'bottom':
			to = ordered.length - 1;
			break;
		case 'up':
			to = from - 1;
			break;
		case 'down':
			to = from + 1;
			break;
	}
	to = Math.max(0, Math.min(ordered.length - 1, to));
	if (to === from) return ordered;

	const next = [...ordered];
	const [moved] = next.splice(from, 1);
	next.splice(to, 0, moved);
	return next;
}

/**
 * Compute the `sort_order` updates to move `itemId` within `ordered`
 * (the group's items in their current displayed order) one step or to an
 * end, per `dir`. Returns only the rows whose `sort_order` changes, each
 * carrying the item and its new dense index. Empty when the move is a
 * no-op (item already at the target edge, or not found).
 */
export function reorderGroup(
	ordered: Item[],
	itemId: string,
	dir: ReorderDirection
): ReorderUpdate[] {
	const next = reorderedList(ordered, itemId, dir);
	if (next === ordered) return [];

	// Dense reindex the whole group, then keep only rows that moved. The
	// dense reindex is what makes this collision-safe: even if every item
	// currently shares sort_order=0 (a freshly-created collection), the
	// result assigns distinct 0..n-1 values in the new visual order.
	const updates: ReorderUpdate[] = [];
	next.forEach((item, index) => {
		if (item.sort_order !== index) {
			updates.push({ item, sort_order: index });
		}
	});
	return updates;
}

/**
 * Return the value of the column immediately to the left/right of `current`
 * in `columnOrder` (the board's left→right display order), or `null` when
 * there is no neighbour in that direction: `current` is at the relevant edge
 * (first + `left`, last + `right`), isn't present in `columnOrder`, or the
 * board has a single column. Pure — used by the kanban card menu's
 * Move left / Move right (TASK-1908) to pick the adjacent column and to
 * decide when the edge option should be hidden.
 */
export function adjacentColumn(
	columnOrder: string[],
	current: string,
	dir: 'left' | 'right'
): string | null {
	const idx = columnOrder.indexOf(current);
	if (idx === -1) return null;
	const target = dir === 'left' ? idx - 1 : idx + 1;
	if (target < 0 || target >= columnOrder.length) return null;
	return columnOrder[target];
}

/**
 * Which directions are unavailable for the item at `index` in a group of
 * `length` items — used to hide no-op menu entries (first item can't move
 * up / to top; last can't move down / to bottom; a lone item moves
 * nowhere).
 */
export function disabledDirections(index: number, length: number): Set<ReorderDirection> {
	const disabled = new Set<ReorderDirection>();
	if (index <= 0) {
		disabled.add('up');
		disabled.add('top');
	}
	if (index >= length - 1) {
		disabled.add('down');
		disabled.add('bottom');
	}
	return disabled;
}
