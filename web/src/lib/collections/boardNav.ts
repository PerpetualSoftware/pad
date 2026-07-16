// Board (kanban) keyboard-navigation model (PLAN-2105 / TASK-2119).
//
// The collection page's j/k/arrow navigation moves a `focusedIndex` over the
// FLAT `filteredItems` array — correct for the list/table views, but the board
// renders those same items grouped into columns (swimlanes) and sorted WITHIN
// each column, so a flat step jumps across columns and feels "all over the
// place". This helper maps a directional keypress onto the board's ACTUAL
// rendered order.
//
// Critical: the caller must pass the columns in the board's real render order
// (column order + within-column sort as BoardView renders them) — BoardView is
// the single source of truth and surfaces that structure upward, so this never
// re-derives a grouping that could drift from the render.

export interface BoardNavColumn<T extends { id: string } = { id: string }> {
	value: string;
	items: T[];
}

export type BoardNavDirection = 'up' | 'down' | 'left' | 'right';

// Given the board columns in visual render order (each with its items in
// rendered order) plus the currently focused item id, return the id of the item
// to focus after a directional keypress — or null when the move isn't possible
// (already at a column edge with no adjacent non-empty column, or no items at
// all).
//
// - up/down move WITHIN the focused item's column, clamped at the column ends
//   (never jump to another column).
// - left/right switch to the nearest NON-EMPTY column in that direction,
//   landing on the same row position clamped to that column's length.
// - With nothing focused yet (or a focused id that's no longer on the board),
//   ANY direction lands on the first item of the first non-empty column.
export function boardKeyNav<T extends { id: string }>(
	columns: BoardNavColumn<T>[],
	focusedId: string | null,
	direction: BoardNavDirection,
): string | null {
	// Locate the focused item within the rendered structure.
	let colIdx = -1;
	let rowIdx = -1;
	if (focusedId != null) {
		for (let c = 0; c < columns.length; c++) {
			const r = columns[c].items.findIndex((it) => it.id === focusedId);
			if (r !== -1) {
				colIdx = c;
				rowIdx = r;
				break;
			}
		}
	}

	// Nothing focused (or the focused item left the board): first nav lands on
	// the first item of the first non-empty column.
	if (colIdx === -1) {
		return firstItemId(columns);
	}

	switch (direction) {
		case 'down': {
			const col = columns[colIdx].items;
			return col[Math.min(rowIdx + 1, col.length - 1)]?.id ?? null;
		}
		case 'up': {
			const col = columns[colIdx].items;
			return col[Math.max(rowIdx - 1, 0)]?.id ?? null;
		}
		case 'right': {
			for (let c = colIdx + 1; c < columns.length; c++) {
				const col = columns[c].items;
				if (col.length > 0) return col[Math.min(rowIdx, col.length - 1)].id;
			}
			return null; // no non-empty column to the right
		}
		case 'left': {
			for (let c = colIdx - 1; c >= 0; c--) {
				const col = columns[c].items;
				if (col.length > 0) return col[Math.min(rowIdx, col.length - 1)].id;
			}
			return null; // no non-empty column to the left
		}
	}
	return null;
}

function firstItemId<T extends { id: string }>(columns: BoardNavColumn<T>[]): string | null {
	for (const col of columns) {
		if (col.items.length > 0) return col.items[0].id;
	}
	return null;
}
