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
//
// Two slots per user and item (BUG-3863). Every producer writes the PENDING
// slot. A draft the pane is showing that was kept before this tab's keeper
// started is moved to the HELD slot, which no producer writes or clears, so
// newer text kept while the notice is up cannot replace the only stored copy
// of the text the user has not resolved yet. Only the notice's own Restore,
// Discard and 'clear' arm remove a held draft; the TTL, the boot sweep and the
// sign-out purge cover it like any other, since it shares the per-user prefix.

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
	/**
	 * The server may hold text the base lacks (BUG-3850, lead rulings baa63dc3
	 * and a6380212): a remote update or this tab's own send came after it, or
	 * there was no confirmed snapshot. Restore never patches from it; it shows
	 * the conflict notice instead.
	 */
	baseStale?: true;
	/**
	 * The crash keeper that kept it (BUG-3863 two-tab ruling): one id per live
	 * keeper, so a keeper can still flag or confirm its own draft after another
	 * tab moved it to the held slot. Absent on every other producer's draft.
	 */
	keeper?: string;
}

const PREFIX = 'pad:refused-raw-draft:';
const HELD_SUFFIX = ':held';

/** Which of an item's two kept drafts (BUG-3863). */
export type RefusedRawDraftSlot = 'pending' | 'held';

/**
 * How long kept text is offered (BUG-3571 part 2). Before part 2 a kept draft
 * lived until it was restored or dismissed; it now also holds text kept for a
 * user who was signed out under it, which must not sit on a shared machine
 * indefinitely. An expired entry reads as none and is removed on that read.
 */
export const REFUSED_RAW_DRAFT_TTL_MS = 7 * 24 * 60 * 60 * 1000;

function keyFor(userId: string, itemId: string, slot: RefusedRawDraftSlot = 'pending'): string {
	return `${PREFIX}${userId}:${itemId}${slot === 'held' ? HELD_SUFFIX : ''}`;
}

/**
 * An item id that could alias another key is refused (BUG-3863): 'X:held'
 * would otherwise read and write X's held slot. Real ids are UUIDs.
 */
function usable(userId: string, itemId: string): boolean {
	return !!userId && !!itemId && !itemId.includes(':');
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
	base?: { seq: number; body: string; stale?: boolean; keeper?: string },
): boolean {
	if (!usable(userId, itemId)) return false;
	const value = JSON.stringify(
		base
			? { markdown, savedAt: now, baseSeq: base.seq, baseBody: base.body, ...(base.stale ? { baseStale: true } : {}), ...(base.keeper ? { keeper: base.keeper } : {}) }
			: { markdown, savedAt: now },
	);
	return writeSlot(userId, itemId, 'pending', value, now);
}

function writeSlot(userId: string, itemId: string, slot: RefusedRawDraftSlot, value: string, now: number): boolean {
	const s = storage();
	if (!s) return false;
	try {
		s.setItem(keyFor(userId, itemId, slot), value);
		return true;
	} catch {
		// Full (TASK-3606): make room from expired drafts and this user's
		// oldest others, then try once more. Blocked or still full: the save
		// still goes out; only the recovery is lost.
		try {
			sweepExpiredRefusedRawDrafts(now);
			evictOldestForUser(userId, itemId);
			s.setItem(keyFor(userId, itemId, slot), value);
			return true;
		} catch {
			return false;
		}
	}
}

/** Remove this user's oldest kept draft other than `keepItemId`'s (either slot). */
function evictOldestForUser(userId: string, keepItemId: string): void {
	const s = storage();
	if (!s) return;
	const prefix = `${PREFIX}${userId}:`;
	const keep = [keyFor(userId, keepItemId), keyFor(userId, keepItemId, 'held')];
	let oldestKey: string | null = null;
	let oldestAt = Infinity;
	for (let i = 0; i < s.length; i++) {
		const k = s.key(i);
		if (k === null || !k.startsWith(prefix) || keep.includes(k)) continue;
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
export function readRefusedRawDraft(
	userId: string,
	itemId: string,
	now = Date.now(),
	slot: RefusedRawDraftSlot = 'pending',
): RefusedRawDraft | null {
	if (!usable(userId, itemId)) return null;
	try {
		const s = storage();
		const raw = s?.getItem(keyFor(userId, itemId, slot));
		if (!raw) return null;
		const parsed = JSON.parse(raw) as Partial<RefusedRawDraft>;
		if (typeof parsed.markdown !== 'string' || typeof parsed.savedAt !== 'number') return null;
		if (now - parsed.savedAt > REFUSED_RAW_DRAFT_TTL_MS) {
			s?.removeItem(keyFor(userId, itemId, slot));
			return null;
		}
		const out: RefusedRawDraft = { markdown: parsed.markdown, savedAt: parsed.savedAt };
		if (typeof parsed.baseSeq === 'number') out.baseSeq = parsed.baseSeq;
		if (typeof parsed.baseBody === 'string') out.baseBody = parsed.baseBody;
		if (parsed.baseStale === true) out.baseStale = true;
		if (typeof parsed.keeper === 'string' && parsed.keeper) out.keeper = parsed.keeper;
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
export function clearRefusedRawDraft(
	userId: string,
	itemId: string,
	onlyIf?: string,
	slot: RefusedRawDraftSlot = 'pending',
): void {
	if (!usable(userId, itemId)) return;
	try {
		const s = storage();
		if (!s) return;
		if (onlyIf !== undefined) {
			const current = readRefusedRawDraft(userId, itemId, Date.now(), slot);
			if (!current || current.markdown !== onlyIf) return;
		}
		s.removeItem(keyFor(userId, itemId, slot));
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

/**
 * The pane's whole offer step for an item it shows (BUG-3863): the draft to
 * show and the slot it lives in, or null. A held draft comes first. A draft
 * equal to the stored body is cleared silently from its slot ('clear'). A
 * pending draft is moved to the held slot before it is shown, unless
 * `keeperOwns` says it is the text this tab's live crash keeper is tracking:
 * the keeper is authoritative for its own in-session draft (lead ruling 1), so
 * that one stays pending. When the held write fails (storage full or blocked)
 * the draft is shown from pending, as before the held slot existed.
 */
export function offerKeptDraft(
	userId: string,
	itemId: string,
	storedBody: string,
	keeperOwns: (markdown: string) => boolean,
	now = Date.now(),
): { draft: RefusedRawDraft; slot: RefusedRawDraftSlot } | null {
	if (!usable(userId, itemId)) return null;
	const held = readRefusedRawDraft(userId, itemId, now, 'held');
	if (held) {
		if (refusedRawDraftOffer(held, storedBody) === 'offer') {
			// A pending copy of the same text is a hold a crash cut short.
			clearRefusedRawDraft(userId, itemId, held.markdown);
			return { draft: held, slot: 'held' };
		}
		clearRefusedRawDraft(userId, itemId, held.markdown, 'held');
	}
	const pending = readRefusedRawDraft(userId, itemId, now);
	const offer = refusedRawDraftOffer(pending, storedBody);
	if (offer === 'clear') clearRefusedRawDraft(userId, itemId, pending!.markdown);
	if (offer !== 'offer') return null;
	const draft = pending!;
	if (keeperOwns(draft.markdown)) return { draft, slot: 'pending' };
	// Held first, then the pending copy goes: a crash between the two leaves
	// both, and the next offer clears the duplicate above.
	if (!writeSlot(userId, itemId, 'held', JSON.stringify(draft), now)) return { draft, slot: 'pending' };
	clearRefusedRawDraft(userId, itemId, draft.markdown);
	return { draft, slot: 'held' };
}

/**
 * The user resolved the shown draft `markdown` (Restore landed, Discard, or
 * nothing to restore): remove it from whichever slot holds it, and only while
 * it is still that text (BUG-3863, lead condition c).
 */
export function resolveKeptDraft(userId: string, itemId: string, markdown: string): void {
	clearRefusedRawDraft(userId, itemId, markdown, 'held');
	clearRefusedRawDraft(userId, itemId, markdown);
}

/*
 * The two exceptions to "producers never touch the held slot" (BUG-3863,
 * two-tab ruling). Another tab may hold a draft a keeper in THIS tab kept and
 * is still tracking, out of the keeper's pending slot. The keeper reaches that
 * held copy by its id, never by its text: the user may have typed on since.
 * A draft with another id, or with none (an earlier tab's D, other
 * producers, entries from before the id existed), is never touched.
 */

/** This keeper sent: a held draft it kept is now stale, whatever its text. */
export function flagHeldDraftStale(userId: string, itemId: string, keeper: string, now = Date.now()): void {
	if (!keeper) return;
	const held = readRefusedRawDraft(userId, itemId, now, 'held');
	if (!held || held.keeper !== keeper || held.baseStale) return;
	writeSlot(userId, itemId, 'held', JSON.stringify({ ...held, baseStale: true }), now);
}

/** This keeper's edits are confirmed as `body`: a held draft it kept with exactly that text goes. */
export function clearHeldDraftIfConfirmed(userId: string, itemId: string, keeper: string, body: string): void {
	if (!keeper) return;
	const held = readRefusedRawDraft(userId, itemId, Date.now(), 'held');
	if (!held || held.keeper !== keeper || held.markdown !== body) return;
	clearRefusedRawDraft(userId, itemId, body, 'held');
}
