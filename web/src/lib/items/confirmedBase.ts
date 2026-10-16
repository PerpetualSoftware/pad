// The base a rich-mode crash draft is edited from (BUG-3850). A crash draft's
// markdown is the editor's serialization; a flush or the server materializer
// writes the body in that same serialization. The raw row body (item.content)
// is not: a body written through the CLI or MCP can end in a newline, keep an
// autolink's angle brackets, or (after a patch and its undo in the open
// editor) still hold text the editor no longer has. Anchoring the draft's
// changes on the raw body then misses, and restore reports a false overlap.
//
// So the base is the editor's own snapshot, taken at the last moment the
// provider had nothing unconfirmed: synced, no unsent or owed edits, no send
// awaiting its barrier (editsMayBeMissing, not editsAtRiskOnClose, which
// leaves the barrier wait out and would let a base swallow just-typed text).
// Everything in it is in the op-log, so any body a flush or the materializer
// writes descends from it. It is refreshed on the keeper's tick only in that
// state and only when the document moved.

export interface ConfirmedBaseSource {
	/** The context the snapshot belongs to (an item's collab session), or null. */
	context: () => object | null;
	/** True only when the server has every edit this tab made. */
	confirmed: () => boolean;
	/** Changes whenever the document may have (the provider's docVersion). */
	version: () => unknown;
	/** The editor's markdown now, or null if unavailable. */
	snapshot: () => string | null;
	/** The item's seq now. */
	seq: () => number;
}

export interface ConfirmedBase {
	markdown: string;
	seq: number;
}

export interface ConfirmedBaseTracker {
	/** Take a new snapshot if the document is confirmed and has moved. */
	refresh: () => void;
	/** The last confirmed snapshot for the current context, or null. */
	current: () => ConfirmedBase | null;
}

export function createConfirmedBaseTracker(source: ConfirmedBaseSource): ConfirmedBaseTracker {
	let held: { context: object; version: unknown; base: ConfirmedBase } | null = null;

	return {
		refresh() {
			const context = source.context();
			if (!context) {
				held = null;
				return;
			}
			if (!source.confirmed()) return;
			const version = source.version();
			if (held && held.context === context && held.version === version) return;
			const markdown = source.snapshot();
			if (markdown === null) return;
			held = { context, version, base: { markdown, seq: source.seq() } };
		},
		current() {
			const context = source.context();
			return held && context && held.context === context ? held.base : null;
		},
	};
}
