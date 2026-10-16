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
// writes descends from it. It is refreshed only in that state and only when
// the document moved, at three moments: the keeper's tick; the provider's
// barrier ack (onConfirmed), before any later keystroke joins the document;
// and the editor's beforeTransaction for a LOCAL change, while the editor
// still holds the document as it was, so a collaborator's text that arrived
// since is in the base before the user's first keystroke on top of it. Both
// matter: a base that lacks text the server holds sends that text again, and
// an anchor is a substring, so typing on the last line duplicates it ('A' →
// 'A C U' over a stored 'A C' restores as 'A C U C'; lead ruling c9db9612 on
// BUG-3850). Remote changes cost nothing here: the version check makes the
// first local change after them the only serialization.

import { ySyncPluginKey } from '@tiptap/y-tiptap';

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

/** The slice of a Tiptap editor the hook needs. */
export interface BeforeTransactionSource {
	on(event: 'beforeTransaction', fn: (props: { transaction: LocalChangeProbe }) => void): unknown;
	off(event: 'beforeTransaction', fn: (props: { transaction: LocalChangeProbe }) => void): unknown;
}
/** The slice of a ProseMirror transaction the hook reads. */
export interface LocalChangeProbe {
	docChanged: boolean;
	getMeta(key: unknown): unknown;
}

/**
 * Call `refresh` before every LOCAL document change, while the editor still
 * holds the document as it was (Tiptap emits beforeTransaction before it
 * applies the new state, and y-prosemirror writes the change to the Y.Doc
 * only after). A change the collab binding applies from a remote update is
 * skipped: refreshing then would serialize on every incoming message.
 */
export function registerRefreshBeforeLocalChange(editor: BeforeTransactionSource, refresh: () => void): () => void {
	const handler = ({ transaction }: { transaction: LocalChangeProbe }) => {
		if (!transaction.docChanged) return;
		const sync = transaction.getMeta(ySyncPluginKey) as { isChangeOrigin?: boolean } | undefined;
		if (sync?.isChangeOrigin) return;
		refresh();
	};
	editor.on('beforeTransaction', handler);
	return () => editor.off('beforeTransaction', handler);
}
