// Raw markdown a save could not store, kept in this browser (BUG-3230 U0).
//
// The item pane's raw-markdown saves ask the server to refuse rather than
// replace edits another tab has not stored yet (`refuse_pending_edits`). In the
// foreground that refusal is a choice for the user (pendingEditsDialog). The
// unload flush has no user to ask and may have no page left to hear the
// answer, so its text is kept HERE before the request goes out, removed only
// when a response says it landed, and offered on the next open of the item.
//
// Keyed by user and item: a different account signing in on this browser must
// never be offered another account's text. Every storage access is guarded,
// because storage can be blocked (a private window) or full, and a failure
// here must never break a save.

export interface RefusedRawDraft {
	markdown: string;
	/** Epoch milliseconds when the text was kept. */
	savedAt: number;
}

const PREFIX = 'pad:refused-raw-draft:';

function keyFor(userId: string, itemId: string): string {
	return `${PREFIX}${userId}:${itemId}`;
}

function storage(): Storage | null {
	try {
		return typeof localStorage === 'undefined' ? null : localStorage;
	} catch {
		return null;
	}
}

/** Keep `markdown` for this user and item, replacing any earlier text. */
export function keepRefusedRawDraft(userId: string, itemId: string, markdown: string, now = Date.now()): void {
	if (!userId || !itemId) return;
	try {
		storage()?.setItem(keyFor(userId, itemId), JSON.stringify({ markdown, savedAt: now }));
	} catch {
		// Blocked or full: the save still goes out; only the recovery is lost.
	}
}

/** The kept text for this user and item, or null. A malformed entry reads as none. */
export function readRefusedRawDraft(userId: string, itemId: string): RefusedRawDraft | null {
	if (!userId || !itemId) return null;
	try {
		const raw = storage()?.getItem(keyFor(userId, itemId));
		if (!raw) return null;
		const parsed = JSON.parse(raw) as Partial<RefusedRawDraft>;
		if (typeof parsed.markdown !== 'string' || typeof parsed.savedAt !== 'number') return null;
		return { markdown: parsed.markdown, savedAt: parsed.savedAt };
	} catch {
		return null;
	}
}

/**
 * Remove the kept text. With `onlyIf`, remove it only when it is still that
 * text: a later unload may have kept newer text for the same item, and an
 * earlier request's success says nothing about it.
 */
export function clearRefusedRawDraft(userId: string, itemId: string, onlyIf?: string): void {
	if (!userId || !itemId) return;
	try {
		const s = storage();
		if (!s) return;
		if (onlyIf !== undefined) {
			const current = readRefusedRawDraft(userId, itemId);
			if (!current || current.markdown !== onlyIf) return;
		}
		s.removeItem(keyFor(userId, itemId));
	} catch {
		// Nothing to recover from.
	}
}

/**
 * What the pane should do with a kept draft when it shows the item: nothing
 * kept, or the kept text is the stored body already (the unload request landed
 * and its page never heard), which clears it silently; otherwise offer it.
 */
export function refusedRawDraftOffer(
	draft: RefusedRawDraft | null,
	storedBody: string,
): 'none' | 'clear' | 'offer' {
	if (!draft) return 'none';
	return draft.markdown === storedBody ? 'clear' : 'offer';
}
