// Shared ESC precedence chain (PLAN-2105 / TASK-2118).
//
// Several overlapping UI layers can each want to handle the ESC key at the
// same time — a right-docked detail pane, a dependency-graph drawer floating
// over it, and the collection list's keyboard-focus marker. Each historically
// registered its OWN `window` keydown listener, so a single ESC fired ALL of
// them and collapsed multiple layers in one press.
//
// This module centralises that contention. Every layer REGISTERS a handler
// (with a priority) when it opens and UNREGISTERS on close/destroy. A single
// top-level keydown listener calls `runTopEscape()`, which invokes only the
// highest-priority registered handler that CONSUMES the key — so one ESC
// closes exactly one layer, innermost-first. A handler returns `true` to
// consume the ESC (stop here) or `false` to decline (fall through to the next
// lower-priority handler), which lets an always-registered layer (e.g. the
// list-focus marker) opt out when it currently has nothing to close.
//
// It is deliberately a plain module (no runes): nothing renders reactively off
// the stack — components push/pop imperatively and one listener reads the top
// — so keeping it non-reactive makes it trivially unit-testable.

export type EscapeHandler = () => boolean;

// Higher priority = more "inner" = handled first. Gaps are intentional so a
// future layer can slot between two without renumbering the others.
export const ESCAPE_PRIORITY = {
	listFocus: 10,
	pane: 20,
	graphDrawer: 30,
} as const;

interface Entry {
	id: number;
	priority: number;
	handler: EscapeHandler;
}

let entries: Entry[] = [];
let nextId = 1;

// Register an ESC handler at the given priority. Returns an unregister fn to
// call on close/destroy (idempotent — safe to call more than once). Ties
// (equal priority) break toward the most-recently registered handler.
export function pushEscapeHandler(handler: EscapeHandler, priority: number): () => void {
	const id = nextId++;
	entries.push({ id, priority, handler });
	return () => {
		entries = entries.filter((e) => e.id !== id);
	};
}

// Invoke the highest-priority handler that consumes the ESC. Returns true if
// some handler consumed it (the caller should then `preventDefault`), false if
// every handler declined or the stack is empty (leave native ESC untouched).
export function runTopEscape(): boolean {
	// Iterate over a COPY, highest priority first (newest wins ties), so a
	// handler that unregisters itself (or another) during its own invocation
	// can't corrupt the iteration.
	const ordered = [...entries].sort((a, b) => b.priority - a.priority || b.id - a.id);
	for (const entry of ordered) {
		if (entry.handler()) return true;
	}
	return false;
}

// Test-only: reset module state between cases.
export function _resetEscapeStackForTests(): void {
	entries = [];
	nextId = 1;
}
