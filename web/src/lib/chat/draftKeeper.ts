// Keeps a half-typed message across a session-expiry redirect (TASK-4044 PR 3b,
// plan Q7). The redirect to /login does not change the identity, so the draft
// would simply vanish. The stash is keyed by user id AND conversation number: a
// draft is only ever offered to the user who typed it, on the thread it was
// typed in (N8), and is removed when it is taken back.

const key = (userId: string, n: number) => `pad:chat-draft:${userId}:${n}`;

/** Stash `text` for `userId` on thread `n`. Silent on a blocked or full store. */
export function keepDraft(userId: string, n: number, text: string): void {
	if (!userId || text === '') return;
	try {
		sessionStorage.setItem(key(userId, n), text);
	} catch {
		// Only this draft is lost.
	}
}

/** The kept draft for this user on this thread, removed as it is read; '' if none. */
export function takeDraft(userId: string, n: number): string {
	if (!userId) return '';
	try {
		const text = sessionStorage.getItem(key(userId, n)) ?? '';
		sessionStorage.removeItem(key(userId, n));
		return text;
	} catch {
		return '';
	}
}
