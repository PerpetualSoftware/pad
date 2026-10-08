/**
 * List / table keyboard navigation (BUG-3492): one j/k step over the rows the
 * view RENDERS, in on-screen order, as `boardKeyNav` steps the board's.
 *
 * The page used to step `filteredItems`, which is in updated order, while the
 * list renders each group in its sort mode's order (manual: stored sort_order,
 * then created_at). After an edit to an older item the two disagree, and j
 * went to the next row by update time rather than the row below.
 *
 * From no focus, or a focused row that is not on screen (a collapsed group),
 * the first row; at either end it stays put. Null when nothing is rendered.
 */
export function listKeyNav(
	order: readonly string[],
	focusedId: string | null,
	step: 1 | -1,
): string | null {
	if (order.length === 0) return null;
	const pos = focusedId ? order.indexOf(focusedId) : -1;
	if (pos < 0) return order[0];
	return order[Math.min(Math.max(pos + step, 0), order.length - 1)];
}
