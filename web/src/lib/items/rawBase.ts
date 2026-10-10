// The raw markdown editor's base (BUG-3540): the row version its text was
// seeded from, sent as `expected_seq` on every raw save so a save over a body
// someone else changed is refused (409 update_conflict) instead of silently
// replacing it. Kept out of ItemDetail so the rules are unit-testable and the
// component gains no new state machine.
//
// A stale seq means the ROW changed, not necessarily the BODY: this tab's own
// overlapping saves, a field-only edit by someone else, or an open tab's flush
// of our own text all bump it. So a refusal is answered by reading the item
// and asking isOwnBody(); only a body this tab already has (its base, or text
// it sent) rebases and resends, once. Anything else goes to the user.

export interface RawBase {
	/** item.seq of the row the raw text was seeded from (or the last save's). */
	seq: number;
	/** The text the raw editor started from at that seq. */
	content: string;
}

/** How many recently sent texts are remembered as this tab's own. */
const SENT_MAX = 8;

export interface RawBaseTracker {
	readonly current: RawBase | null;
	/** Entering raw mode: the text the editor starts from, at the row's seq then. */
	seed(seq: number, shown: string): void;
	/** An edit: sets the base only if none is held (raw entered without a seed). */
	noteEdit(seq: number, shown: string): void;
	/** A save is going out with this text. */
	noteSent(text: string): void;
	/** A save landed: the row is now at `seq`, holding `sent`. */
	landed(seq: number, sent: string): void;
	/** The editor is clean again (or the item changed): the next edit re-seeds. */
	reset(): void;
	/** Texts this tab knows to be its own or its base. */
	ownTexts(): string[];
}

export function createRawBase(): RawBaseTracker {
	let base: RawBase | null = null;
	let sent: string[] = [];
	return {
		get current() {
			return base;
		},
		seed(seq, shown) {
			base = { seq, content: shown };
			sent = [];
		},
		noteEdit(seq, shown) {
			if (base === null) base = { seq, content: shown };
		},
		noteSent(text) {
			sent = [...sent.filter((t) => t !== text), text].slice(-SENT_MAX);
		},
		landed(seq, landedText) {
			base = { seq, content: landedText };
		},
		reset() {
			base = null;
			sent = [];
		},
		ownTexts() {
			return base ? [base.content, ...sent] : [...sent];
		},
	};
}

/**
 * Whether `stored` is a body this tab already has: EXACTLY one of `own`, or
 * exactly the editor's canonical form of one (when a canonicalizer is
 * available). Never a looser match: a body that differs by whitespace the
 * canonicalizer does not normalise is someone else's change, and the user is
 * asked (lead ruling, BUG-3540): a false dialog is cheap, a silent overwrite
 * is not.
 */
export function isOwnBody(
	stored: string,
	own: readonly string[],
	canonicalize?: (markdown: string) => string | null,
): boolean {
	for (const text of own) {
		if (stored === text) return true;
		if (canonicalize) {
			const c = canonicalize(text);
			if (c !== null && stored === c) return true;
		}
	}
	return false;
}
