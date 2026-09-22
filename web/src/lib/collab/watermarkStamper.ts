// The page half of BUG-3124 unit B's content-free watermark stamp, kept out of
// ItemDetail so the component gains no new async continuation (its identity
// gate audits every one) and so the rules below are unit-testable.
//
// Called with the content a deduped flush proved the server holds. It sends
// the tab's op-log cursor and the sha256 of that content; the server stamps
// only if cursor == MAX(op-log id) and the hash matches items.content, both
// re-checked atomically, and writes nothing else. This side only decides
// whether sending is worth it.

import { sha256Hex } from '$lib/utils/sha256';

export interface WatermarkStampInput {
	ws: string;
	itemId: string;
	content: string;
	keepalive: boolean;
}

export interface WatermarkStamperDeps {
	/** The op-log cursor of the provider for `itemId`, or 0 if none tracks it. */
	cursorFor: (itemId: string) => number;
	/** True while force-refresh recovery is in flight: the Y.Doc is known stale. */
	isRecovering: () => boolean;
	/** Resolves with the server's answer; `advanced: false` means the proof did
	 *  not hold at commit time (cursor behind MAX, or the body changed). */
	send: (
		ws: string,
		itemId: string,
		cursor: number,
		contentSHA256: string,
		keepalive: boolean,
	) => Promise<{ advanced?: boolean } | void>;
}

export function createWatermarkStamper(deps: WatermarkStamperDeps): (input: WatermarkStampInput) => void {
	// Highest cursor already sent per item. Plain Map, not $state: a handler-only
	// tracker (CONVE-1688).
	const lastStamped = new Map<string, number>();
	return ({ ws, itemId, content, keepalive }) => {
		if (deps.isRecovering()) return;
		const cursor = deps.cursorFor(itemId);
		// A cursor the server never issued: nothing to prove.
		if (cursor < 1) return;
		// A cursor already sent is skipped — EXCEPT on the keepalive (pagehide)
		// path, which always sends: an earlier ordinary request for the same
		// cursor may still be in flight and be cancelled by the teardown, and
		// nothing would trigger a retry after the page is gone (codex round 1).
		// A duplicate stamp is a no-op on the server.
		if (!keepalive && cursor <= (lastStamped.get(itemId) ?? 0)) return;
		lastStamped.set(itemId, cursor);
		// Synchronous up to the request (sha256Hex, not crypto.subtle), so the
		// pagehide path starts its keepalive request before the page unloads.
		const forget = () => {
			// Forget the cursor so a later settle can retry. The stamp refines a
			// signal; it is never a write the user owns, so a failure is silent.
			if (lastStamped.get(itemId) === cursor) lastStamped.delete(itemId);
		};
		// A SYNCHRONOUS throw is caught too: this runs inside the flusher's
		// dedupe arm, and a throw there would reject the flush itself — whose
		// 'deduped' result callers such as the rich→raw toggle await.
		try {
			deps
				.send(ws, itemId, cursor, sha256Hex(content), keepalive)
				.then((res) => {
					// A 200 that did not advance is not a success for this cursor:
					// the body or the op-log moved under the proof. Forget it so a
					// later flush can try again (codex round 1).
					if (res && res.advanced === false) forget();
				}, forget);
		} catch {
			forget();
		}
	};
}
