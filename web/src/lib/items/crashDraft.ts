// A crash-safe recovery draft (TASK-3606). The unload prompt and the teardown
// flush never run when the tab dies outright (a mobile process kill, a browser
// crash), so while an open item holds edits the server may not have, its
// markdown is kept every couple of seconds in the kept-draft store
// (refusedRawDraft), under the user and item. The next open of that item offers
// it back through the existing offer and its expected_seq restore.
//
// Cleared only on positive confirmation: the SAME item reports nothing at risk
// any more (the barrier ack), and the kept text is still the one written here.
// Switching items clears nothing; a draft whose edits did land reads as the
// stored body on the next open and is cleared there silently ('clear' arm).

import { keepRefusedRawDraft, clearRefusedRawDraft } from './refusedRawDraft';

/** How often the keeper looks while it is registered. */
export const CRASH_DRAFT_INTERVAL_MS = 2000;
/** Bodies above this are not kept (6x the largest measured; design note). */
export const CRASH_DRAFT_MAX_CHARS = 1_000_000;

export interface CrashDraftSource {
	/** The signed-in user the text belongs to; '' means do nothing. */
	userId: () => string;
	/** The item shown now, or null. */
	itemId: () => string | null;
	/** Whether that item holds edits the server may not have. */
	atRisk: () => boolean;
	/** Its markdown and the seq/body it was edited from, or null if unavailable. */
	snapshot: () => { markdown: string; baseSeq: number; baseBody: string; baseStale?: boolean } | null;
	/** Called on every tick before anything else (BUG-3850: the confirmed base). */
	observe?: () => void;
	/** True while a confirmed sign-out is discarding: keep nothing then. */
	discarding?: () => boolean;
	/**
	 * A cheap value that changes whenever the text may have (codex r1): the
	 * keeper serializes only when it differs from the last write's, so an
	 * unchanged large document is not re-serialized every tick.
	 */
	version?: () => unknown;
	/**
	 * Subscribe to "a local update is about to be sent" (BUG-3850 r8 hunter-1).
	 * The server may hold it from then on, so the draft kept here is rewritten
	 * flagged stale at once: the next tick may never come, and on a reconnect's
	 * catch-up it would clear the draft rather than flag it.
	 */
	onSent?: (fn: () => void) => () => void;
}

/**
 * Start keeping crash drafts for `source`; the returned function stops it.
 * `schedule` is injectable for tests (defaults to setInterval).
 */
export function registerCrashDraftKeeper(
	source: CrashDraftSource,
	schedule: (fn: () => void, ms: number) => () => void = (fn, ms) => {
		const t = setInterval(fn, ms);
		return () => clearInterval(t);
	},
): () => void {
	let last: { userId: string; itemId: string; markdown: string; version: unknown; base: string; baseSeq: number; baseBody: string; stale: boolean } | null = null;

	const tick = () => {
		const userId = source.userId();
		const itemId = source.itemId();
		source.observe?.();
		if (!userId || !itemId || source.discarding?.()) return;
		if (source.atRisk()) {
			const version = source.version?.();
			if (version !== undefined && last && last.userId === userId && last.itemId === itemId && last.version === version) return;
			const snap = source.snapshot();
			if (!snap || snap.markdown.length > CRASH_DRAFT_MAX_CHARS) return;
			// The base is part of what was kept: the same text over a base that
			// has since moved, or turned stale, is a different draft (BUG-3850
			// r5: a remote ' C' the user deleted again within one tick leaves the
			// markdown as it was and the base stale).
			const base = JSON.stringify([snap.baseSeq, snap.baseBody, snap.baseStale === true]);
			if (last && last.userId === userId && last.itemId === itemId && last.markdown === snap.markdown && last.base === base) return;
			if (keepRefusedRawDraft(userId, itemId, snap.markdown, Date.now(), { seq: snap.baseSeq, body: snap.baseBody, stale: snap.baseStale })) {
				last = { userId, itemId, markdown: snap.markdown, version, base, baseSeq: snap.baseSeq, baseBody: snap.baseBody, stale: snap.baseStale === true };
			}
			return;
		}
		// Nothing at risk on the SAME item this keeper wrote for: the edits are
		// confirmed, so the draft goes, unless something newer replaced it.
		if (last && last.userId === userId && last.itemId === itemId) {
			clearRefusedRawDraft(userId, itemId, last.markdown);
			last = null;
		}
	};

	const unsubscribe = source.onSent?.(() => {
		if (!last || last.stale || last.userId !== source.userId() || last.itemId !== source.itemId()) return;
		if (keepRefusedRawDraft(last.userId, last.itemId, last.markdown, Date.now(), { seq: last.baseSeq, body: last.baseBody, stale: true })) {
			last = { ...last, stale: true, base: JSON.stringify([last.baseSeq, last.baseBody, true]) };
		}
	});
	const stop = schedule(tick, CRASH_DRAFT_INTERVAL_MS);
	return () => {
		stop();
		unsubscribe?.();
	};
}
