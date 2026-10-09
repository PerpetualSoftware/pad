// A board lane renders a WINDOW of its cards (TASK-2230).
//
// A terminal lane collects every card ever completed (1,000 in a measured
// workspace), and the board mounted every one: 21,351 DOM nodes and a ~500 ms
// long task (1.9-2.2 s on a 4x-throttled CPU) before the first card painted.
// Windowing the DOM (virtualization) is not open to the board, because
// svelte-dnd-action needs every drop target mounted; that is why TASK-1347 only
// skipped paint. So a lane mounts its first CAP cards and a "Show all" control.
//
// THE FULL LANE STAYS THE SOURCE OF TRUTH. The board keeps every card of a lane
// in order and hands the drag zone only the window. A drag reports the window's
// new order, so every write path rebuilds the full lane from it before it is
// persisted: the window's new order, then the hidden tail in its old order.
// Persisting the window alone would number 0..CAP while the tail keeps its
// values, giving two cards one value and an order nobody chose.

/** Cards a terminal lane (done, cancelled, ...) mounts before "Show all". */
export const TERMINAL_LANE_CAP = 50;
/** Cards any other lane mounts before "Show all": a backlog grows the same way. */
export const LANE_CAP = 300;

/** How many cards a lane mounts: everything when uncapped, else its cap. */
export function laneCap(opts: { terminal: boolean; uncapped: boolean }): number {
	if (opts.uncapped) return Infinity;
	return opts.terminal ? TERMINAL_LANE_CAP : LANE_CAP;
}

/** The cards a lane mounts: the first `size` of the full lane. */
export function laneWindow<T>(full: readonly T[], size: number): readonly T[] {
	return size >= full.length ? full : full.slice(0, size);
}

/**
 * The full lane after a drag changed its window: the window's new order, then
 * every card that was hidden and is not in the new window, in its old order.
 * `before` is the window the drag started from; a card that left the window
 * (dragged out) is in neither, and one that arrived (dragged in) is only in
 * `after`, so both come out right.
 */
export function rebuildLane<T extends { id: string }>(
	full: readonly T[],
	before: readonly T[],
	after: readonly T[]
): T[] {
	const shown = new Set(before.map((c) => c.id));
	const now = new Set(after.map((c) => c.id));
	return [...after, ...full.filter((c) => !shown.has(c.id) && !now.has(c.id))];
}
