// Keep at-risk edits before a session-expiry redirect (BUG-3571 part 2).
//
// A 401 on an expired session sends the tab to /login without changing the
// identity epoch (the session is gone server-side; nothing in this tab signed
// out), so the identity listeners that keep a previous user's text never run,
// and the unload flush that follows is sent on a dead session. Each open item
// registers a keeper; the API client's redirect runs them, synchronously,
// before it navigates, so the text is in this browser before the page goes.
//
// No imports on purpose: the API client calls this, and anything importing
// the auth store from here would close a cycle through the client.

export type SessionExpiryKeeper = () => void;

const keepers = new Set<SessionExpiryKeeper>();

/** Register a keeper; the returned function unregisters it. */
export function registerSessionExpiryKeeper(fn: SessionExpiryKeeper): () => void {
	keepers.add(fn);
	return () => {
		keepers.delete(fn);
	};
}

/** Run every keeper. One that throws must not stop the others or the redirect. */
export function runSessionExpiryKeepers(): void {
	for (const fn of [...keepers]) {
		try {
			fn();
		} catch {
			// The redirect goes ahead; only this keeper's text is lost.
		}
	}
}
