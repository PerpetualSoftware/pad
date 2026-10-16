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
// writes descends from it. It is refreshed on the keeper's tick and on the
// provider's barrier ack (onConfirmed), only in that state and only when the
// document moved. The ack refresh matters: a base a tick old lacks text the
// server holds, and an anchor is a substring, so typing on the last line
// would send that text again ('A' → 'A T U' over a stored 'A T').

/** The provider flags `confirmed` reads; a structural slice of CollabProvider. */
export interface ConfirmedProviderState {
	synced: boolean;
	editsMayBeMissing: boolean;
	editsAtRiskOnClose: boolean;
}

/**
 * True only when the server has every edit this tab made: synced (a partly
 * replayed document is no base), and nothing unsent, owed, awaiting its
 * barrier or awaiting a repair.
 */
export function providerConfirmed(p: ConfirmedProviderState | null | undefined): boolean {
	return !!p && p.synced && !p.editsMayBeMissing && !p.editsAtRiskOnClose;
}

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
	/** The stored body as this tab last read it, taken with the snapshot. */
	raw: () => string;
}

export interface ConfirmedBase {
	markdown: string;
	seq: number;
	/** The row read at the same moment, so seq and row are one pair. */
	raw: string;
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
			held = { context, version, base: { markdown, seq: source.seq(), raw: source.raw() } };
		},
		current() {
			const context = source.context();
			return held && context && held.context === context ? held.base : null;
		},
	};
}
