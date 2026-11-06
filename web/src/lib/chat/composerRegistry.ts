// Module-level registries behind a late chat refusal (BUG-4341). A send outlives
// the view that started it: closing the dock or switching conversation disposes
// the sending ChatThread and unmounts its Composer, and reopening builds a NEW
// thread and a NEW composer for the same conversation. So neither lives on a
// thread instance.
//
// - Composers: the live Composer registers for (user, conversation). A refusal
//   that arrives after its sender went away is handed to whichever composer is
//   registered now; with none, the caller writes the draft instead. Never keyed
//   by anything but (user, n), so a refusal cannot reach another conversation.
// - Sends: op_keys whose POST has not answered. open() does not list a stored
//   attempt that is still in flight as "Send not confirmed". No body matching.
//   When such a send then ends without an answer (the network), the thread now
//   open for (user, n) is told, and shows the stored attempt then.
import { authStore } from '$lib/stores/auth.svelte';
import type { SendOutcome } from '$lib/stores/chatThread.svelte';

/** The definite refusals a composer can take back (not sent, failed, stale or not started). */
export type LateOutcome = Exclude<SendOutcome, 'sent' | 'failed' | 'stale' | 'not_started'>;
export type RestoreFn = (body: string, outcome: LateOutcome, replySeq: number | null) => void;

const composers = new Map<string, RestoreFn>();
const sending = new Set<string>();
const views = new Map<string, (opKey: string) => void>();
const slot = (userId: string, n: number) => `${userId}\u0000${n}`;

/** Register the live composer; the returned function unregisters it unless a newer one replaced it. */
export function registerComposer(userId: string, n: number, restore: RestoreFn): () => void {
	const key = slot(userId, n);
	composers.set(key, restore);
	return () => {
		if (composers.get(key) === restore) composers.delete(key);
	};
}

/** True when a live composer took the text back; false when none is registered for (user, n). */
export function deliverRefusal(userId: string, n: number, body: string, outcome: LateOutcome, replySeq: number | null): boolean {
	const restore = composers.get(slot(userId, n));
	if (!restore) return false;
	restore(body, outcome, replySeq);
	return true;
}

export function beginSend(opKey: string): void {
	sending.add(opKey);
}
export function endSend(opKey: string): void {
	sending.delete(opKey);
}
export function isSending(opKey: string): boolean {
	return sending.has(opKey);
}

/** The thread open for (user, n) hears of a send that ended unanswered after it was opened. */
export function registerThreadView(userId: string, n: number, surface: (opKey: string) => void): () => void {
	const key = slot(userId, n);
	views.set(key, surface);
	return () => {
		if (views.get(key) === surface) views.delete(key);
	};
}

/** A send ended without an answer or a refusal: tell the thread open for (user, n), if any. */
export function notifyUnconfirmed(userId: string, n: number, opKey: string): void {
	views.get(slot(userId, n))?.(opKey);
}

export function clearComposerRegistry(): void {
	views.clear();
	composers.clear();
	sending.clear();
}

authStore.onIdentityChange(clearComposerRegistry);
