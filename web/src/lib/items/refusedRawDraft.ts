// Raw markdown a save could not store, kept in this browser (BUG-3230 U0).
//
// The item pane's raw-markdown saves ask the server to refuse rather than
// replace edits another tab has not stored yet (`refuse_pending_edits`). In the
// foreground that refusal is a choice for the user (pendingEditsDialog). The
// unload flush has no user to ask and may have no page left to hear the
// answer, so its text is kept HERE before the request goes out, removed only
// when a response says it landed, and offered on the next open of the item.
//
// BUG-3571 part 2 adds a second producer: text an open item could no longer
// save under its author's session (an identity change the tab did not start,
// or a session-expiry redirect) is kept here under that author's id, rich text
// as its serialized markdown, and offered back the same way. Kept drafts
// expire after REFUSED_RAW_DRAFT_TTL_MS, and an explicit sign-out purges all
// of the user's.
//
// Keyed by user and item: a different account signing in on this browser must
// never be offered another account's text. Every storage access is guarded,
// because storage can be blocked (a private window) or full, and a failure
// here must never break a save.

export interface RefusedRawDraft {
	markdown: string;
	/** Epoch milliseconds when the text was kept. */
	savedAt: number;
	/**
	 * The item's seq and body the edits were made on (TASK-3606, crash drafts
	 * only; absent on older entries and other producers). A seq above it now
	 * means someone saved since, and the base body lets a later restore apply
	 * only the user's changes rather than replace the whole document.
	 */
	baseSeq?: number;
	baseBody?: string;
}

const PREFIX = 'pad:refused-raw-draft:';

/**
 * How long kept text is offered (BUG-3571 part 2). Before part 2 a kept draft
 * lived until it was restored or dismissed; it now also holds text kept for a
 * user who was signed out under it, which must not sit on a shared machine
 * indefinitely. An expired entry reads as none and is removed on that read.
 */
export const REFUSED_RAW_DRAFT_TTL_MS = 7 * 24 * 60 * 60 * 1000;

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
export function keepRefusedRawDraft(
	userId: string,
	itemId: string,
	markdown: string,
	now = Date.now(),
	base?: { seq: number; body: string },
): boolean {
	if (!userId || !itemId) return false;
	const value = JSON.stringify(base ? { markdown, savedAt: now, baseSeq: base.seq, baseBody: base.body } : { markdown, savedAt: now });
	const s = storage();
	if (!s) return false;
	try {
		s.setItem(keyFor(userId, itemId), value);
		return true;
	} catch {
		// Full (TASK-3606): make room from expired drafts and this user's
		// oldest others, then try once more. Blocked or still full: the save
		// still goes out; only the recovery is lost.
		try {
			sweepExpiredRefusedRawDrafts(now);
			evictOldestForUser(userId, itemId);
			s.setItem(keyFor(userId, itemId), value);
			return true;
		} catch {
			return false;
		}
	}
}

/** Remove this user's oldest kept draft other than `keepItemId`'s. */
function evictOldestForUser(userId: string, keepItemId: string): void {
	const s = storage();
	if (!s) return;
	const prefix = `${PREFIX}${userId}:`;
	let oldestKey: string | null = null;
	let oldestAt = Infinity;
	for (let i = 0; i < s.length; i++) {
		const k = s.key(i);
		if (k === null || !k.startsWith(prefix) || k === keyFor(userId, keepItemId)) continue;
		try {
			const at = (JSON.parse(s.getItem(k) ?? '{}') as { savedAt?: number }).savedAt;
			if (typeof at === 'number' && at < oldestAt) {
				oldestAt = at;
				oldestKey = k;
			}
		} catch {
			oldestKey ??= k;
		}
	}
	if (oldestKey) s.removeItem(oldestKey);
}

/**
 * The kept text for this user and item, or null. A malformed entry reads as
 * none; an expired one (older than the TTL) reads as none and is removed.
 */
export function readRefusedRawDraft(userId: string, itemId: string, now = Date.now()): RefusedRawDraft | null {
	if (!userId || !itemId) return null;
	try {
		const s = storage();
		const raw = s?.getItem(keyFor(userId, itemId));
		if (!raw) return null;
		const parsed = JSON.parse(raw) as Partial<RefusedRawDraft>;
		if (typeof parsed.markdown !== 'string' || typeof parsed.savedAt !== 'number') return null;
		if (now - parsed.savedAt > REFUSED_RAW_DRAFT_TTL_MS) {
			s?.removeItem(keyFor(userId, itemId));
			return null;
		}
		const out: RefusedRawDraft = { markdown: parsed.markdown, savedAt: parsed.savedAt };
		if (typeof parsed.baseSeq === 'number') out.baseSeq = parsed.baseSeq;
		if (typeof parsed.baseBody === 'string') out.baseBody = parsed.baseBody;
		return out;
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
 * Remove every kept draft of this user, on every item (BUG-3571 part 2): an
 * explicit sign-out leaves nothing of theirs on this device. Keys are
 * collected before removal, since removing while indexing shifts the indices.
 */
export function purgeRefusedRawDraftsForUser(userId: string): void {
	if (!userId) return;
	try {
		const s = storage();
		if (!s) return;
		const prefix = `${PREFIX}${userId}:`;
		const keys: string[] = [];
		for (let i = 0; i < s.length; i++) {
			const k = s.key(i);
			if (k !== null && k.startsWith(prefix)) keys.push(k);
		}
		for (const k of keys) s.removeItem(k);
	} catch {
		// Nothing to recover from.
	}
}

/**
 * Remove every kept draft past the TTL, for EVERY user, plus any entry that no
 * longer parses (BUG-3571 part 2 follow-up). Expiry otherwise happens only when
 * a draft is read, so a user who never opens that item again on this machine,
 * or never signs in here again, would leave their text in storage past the
 * TTL. Run once at app boot. Keys are collected before removal.
 */
export function sweepExpiredRefusedRawDrafts(now = Date.now()): void {
	try {
		const s = storage();
		if (!s) return;
		const expired: string[] = [];
		for (let i = 0; i < s.length; i++) {
			const k = s.key(i);
			if (k === null || !k.startsWith(PREFIX)) continue;
			let savedAt: unknown;
			try {
				savedAt = (JSON.parse(s.getItem(k) ?? '') as Partial<RefusedRawDraft>).savedAt;
			} catch {
				savedAt = undefined;
			}
			if (typeof savedAt !== 'number' || now - savedAt > REFUSED_RAW_DRAFT_TTL_MS) expired.push(k);
		}
		for (const k of expired) s.removeItem(k);
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
