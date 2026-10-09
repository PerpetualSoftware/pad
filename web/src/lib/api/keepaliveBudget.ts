// The keepalive body budget (BUG-3522).
//
// A fetch with `keepalive: true` is the only request specified to outlive its
// document, and the Fetch spec caps the TOTAL in-flight keepalive body of a
// fetch group at 64 KiB. Over the cap the browser refuses the request outright
// (Chromium rejects with a TypeError before anything is sent), so a teardown
// flush of a large body did not leave the page at all. Measured on BUG-3522:
// an 87,192-byte collab-snapshot PATCH dispatched on a pane switch never
// reached the server.
//
// So a request asks for keepalive only when its body fits a budget below the
// cap, and otherwise goes as an ordinary fetch, which has no size limit:
//   - an in-page teardown (pane close, item switch) runs in a page that
//     survives it, so the ordinary fetch completes;
//   - on unload it is best effort: it may be cancelled with the document. Every
//     editor path that reaches here with edits the server does not hold raises
//     the unsaved-changes prompt (offlineEdits.unloadLosesEdits: unsent collab
//     edits, a dirty raw saver), and collab edits the server does hold are in
//     its op-log, which recovery materializes into items.content (BUG-3521);
//   - visibilitychange to hidden usually leaves the page alive, so it usually
//     completes.
//
// The budget is under 64 KiB because the cap is shared: the unload flushes of
// the raw saver and the collab flusher, the workspace-tab route save and any
// watermark stamp all count against the same 64 KiB.

/** Bytes of request body a single keepalive request may carry. */
export const KEEPALIVE_BODY_BUDGET = 56 * 1024;

/** Whether `body` (the exact request body) fits the keepalive budget, measured
 *  in UTF-8 bytes as the browser counts it. */
export function fitsKeepaliveBudget(body: string): boolean {
	// A cheap upper bound first: UTF-8 is at most 3 bytes per UTF-16 unit.
	if (body.length * 3 <= KEEPALIVE_BODY_BUDGET) return true;
	return new TextEncoder().encode(body).length <= KEEPALIVE_BODY_BUDGET;
}

// Bytes of keepalive body this page has in flight. The 64 KiB cap is the SUM
// over in-flight keepalive requests (codex r1 on BUG-3522): at unload the
// collab flush and the raw saver can each fit alone and still overflow
// together, and the browser then refuses the later one.
let inFlight = 0;

/** Bytes of keepalive body currently in flight (for tests). */
export function keepaliveBytesInFlight(): number {
	return inFlight;
}

/**
 * The `keepalive` flag a request should actually send, and a release to call
 * when it settles. Keepalive is kept only if this body fits the budget
 * TOGETHER with every keepalive body still in flight; otherwise the request
 * goes as an ordinary fetch and says so in the console (never silently).
 */
export function reserveKeepalive(
	wanted: boolean | undefined,
	body: string,
	what: string
): { keepalive: boolean | undefined; release: () => void } {
	const none = { keepalive: wanted, release: () => {} };
	if (!wanted) return none;
	// Exact UTF-8 bytes: an upper bound would over-reserve and push a later
	// small request off keepalive for nothing.
	const bytes = new TextEncoder().encode(body).length;
	if (inFlight + bytes <= KEEPALIVE_BODY_BUDGET) {
		inFlight += bytes;
		let released = false;
		return {
			keepalive: true,
			release: () => {
				if (released) return;
				released = true;
				inFlight -= bytes;
			}
		};
	}
	console.warn(
		`[keepalive] ${what}: ${bytes} bytes with ${inFlight} already in flight is over the ${KEEPALIVE_BODY_BUDGET}-byte keepalive budget; sending as an ordinary request (BUG-3522)`
	);
	return { keepalive: false, release: () => {} };
}
