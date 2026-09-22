/**
 * Ordering for concurrent writes that each REPLACE the same target.
 *
 * PLAN-2857 U4, codex round 8 findings R8-1 / R8-2. A `multi_relation` edit is
 * a WHOLE-LIST write: removing one chip sends the entire remaining list. Two of
 * them in flight at once are therefore not independent deltas the server can
 * merge — each one asserts the complete value, and the LATER one is the only
 * one that carries the user's current intent.
 *
 * TWO COUNTERS, because each single-counter version answers a question that is
 * not the one being asked (harness note `ticket-plus-high-water-request-ordering`,
 * learned across two rounds of BUG-2773):
 *
 *   * `dispatched` — the newest write SENT for a target. Its question is
 *     "does a newer intent for this field exist?", and the caller asks it
 *     before RE-SENDING a body it has already sent once (the parent's 409
 *     refetch-and-retry). Replaying a stale whole list after a newer one has
 *     been sent is the lost update in R8-1, and it is the only place a
 *     dispatch count is the right instrument: a retry is a NEW SEND of an OLD
 *     body, so a newer body always subsumes it. BUG-2773's warning — that
 *     counting dispatches lets a request which later FAILS invalidate a good
 *     in-flight response — does not reach this use, because nothing here
 *     discards a response on the strength of a dispatch; it only declines to
 *     manufacture a fresh send of a body the user has already replaced.
 *
 *   * `applied` — the highest ticket whose result actually WROTE. Its question
 *     is "may this response replace what is on screen?", and it gives
 *     newest-wins among responses that resolve out of order. Counting applies
 *     is wrong for the first question (an older response landing first would
 *     lock the newer one out) exactly as counting dispatches is wrong for the
 *     second, which is why both exist.
 *
 * Deliberately NOT reactive and NOT a store: this is request bookkeeping read
 * inside async handlers, never rendered. Putting it in the effect graph would
 * re-run consumers on every dispatch for no rendered consequence (CONVE-1688's
 * split).
 */

/**
 * A ticket taken at dispatch. Opaque on purpose — the number alone is
 * meaningless without the target it was drawn for, and pairing them here stops
 * a caller comparing a ticket against a different field's counter.
 */
export type WriteTicket = {
	readonly target: string;
	readonly n: number;
};

export class WriteOrder {
	/**
	 * Keyed by whatever string the caller uses to name one replaceable target —
	 * `<item id>` + NUL + `<field key>` in the item pane, the relation identity
	 * stamp in the field editor. Entries are never evicted: two small integers
	 * per target the user has actually edited is a bound set by how much editing
	 * a human does in one tab, and an eviction firing between a dispatch and its
	 * response would reset a live ticket to "newest", which is the one failure
	 * this class exists to prevent.
	 */
	private dispatched = new Map<string, number>();
	private applied = new Map<string, number>();

	/** Take a ticket for a write about to be sent. */
	take(target: string): WriteTicket {
		const n = (this.dispatched.get(target) ?? 0) + 1;
		this.dispatched.set(target, n);
		return { target, n };
	}

	/**
	 * Has a NEWER write for the same target been dispatched since this ticket?
	 *
	 * Ask before re-sending this ticket's body, and before reporting its
	 * failure: a superseded write's error is not news, since the newer write
	 * owns the outcome the user is waiting on.
	 */
	superseded(ticket: WriteTicket): boolean {
		return ticket.n < (this.dispatched.get(ticket.target) ?? 0);
	}

	/**
	 * Claim the right to apply this ticket's result, and record that it did.
	 *
	 * False when something newer has already written — the response is stale,
	 * however healthy it looks. Call it ONCE per ticket, immediately before the
	 * write it guards: it advances the mark itself, so a second call for the
	 * same ticket answers false and would read as a supersession that never
	 * happened.
	 */
	claim(ticket: WriteTicket): boolean {
		if (ticket.n <= (this.applied.get(ticket.target) ?? 0)) return false;
		this.applied.set(ticket.target, ticket.n);
		return true;
	}

	/** The newest ticket number dispatched for a target, or 0. */
	dispatchedFor(target: string): number {
		return this.dispatched.get(target) ?? 0;
	}

	/** The highest ticket that applied for a target, or 0. */
	appliedFor(target: string): number {
		return this.applied.get(target) ?? 0;
	}
}

/**
 * Send one optimistic-concurrency write, refetching and retrying on a conflict,
 * and ABANDONING rather than replaying a body the user has already replaced.
 *
 * The loop itself is BUG-2273's and unchanged in shape: a 409 means the row
 * moved under us, so re-read it and re-send the same single-key patch against
 * the fresh `updated_at`. That is safe while the patch is a change nobody else
 * in this pane is also making. It stops being safe the moment the same field
 * has a NEWER write in flight, because both writes assert the whole value of
 * that one key and the retry would re-assert the older one (R8-1: remove A then
 * B from [A,B,C], and a retry of the first write persists [B,C] — A is back,
 * with no error anywhere).
 *
 * Extracted from the pane (rather than left inline) because this is the defect
 * site, and a 7,900-line component can only be guarded by grepping its source
 * for spellings. Here the loop can be driven directly, conflict by conflict,
 * with the interleaving that produced the lost update.
 */
export async function submitOrderedOCC<T, TToken = string>(opts: {
	order: WriteOrder;
	ticket: WriteTicket;
	/** Attempts AFTER the first, so 2 means at most three sends. */
	maxRetries: number;
	initialExpected: TToken;
	send: (expected: TToken) => Promise<T>;
	refetch: () => Promise<T>;
	/**
	 * The concurrency token carried by a freshly-read row (BUG-3037).
	 *
	 * This used to be hardcoded as `latest.updated_at`, which forced every
	 * caller onto a token with ONE-SECOND resolution: two writes inside one
	 * second both match it, so neither conflicts and the loser silently
	 * overwrites the winner — and the retry below, which exists precisely to
	 * re-apply a delta after a conflict, never runs because no conflict is ever
	 * reported. Items now round-trip `seq` instead, so the extractor is the
	 * caller's.
	 */
	tokenOf: (row: T) => TToken;
	isConflict: (e: unknown) => boolean;
	/** False once the surrounding view has moved on (an item switch). */
	stillCurrent: () => boolean;
	/** The freshest row seen while retrying — the caller's fallback truth. */
	onRefetched?: (latest: T) => void;
}): Promise<T> {
	const { order, ticket, maxRetries, send, refetch, isConflict, stillCurrent, onRefetched, tokenOf } =
		opts;
	let expected = opts.initialExpected;
	for (let attempt = 0; ; attempt++) {
		try {
			return await send(expected);
		} catch (e) {
			if (!isConflict(e) || attempt >= maxRetries || !stillCurrent() || order.superseded(ticket)) {
				throw e;
			}
			const latest = await refetch();
			onRefetched?.(latest);
			expected = tokenOf(latest);
			// BOTH questions again after the refetch, not only before it: the
			// refetch is an await, and both the newer write and the view moving
			// on are things a click lands inside it. Asking only about
			// supersession here sent another PATCH at an item the pane had
			// already left — the caller then discarded the response, so the write
			// was invisible while still landing on the row. Checking here leaves
			// no async gap before the re-send, because `send` starts its request
			// synchronously.
			if (order.superseded(ticket) || !stillCurrent()) throw e;
		}
	}
}

/**
 * The item pane's target key for one field of one item.
 *
 * The item id is IN the key rather than handled by clearing the map on an item
 * switch: the pane bumps its load generation on every refetch of the SAME item
 * too, so a clear would land between a dispatch and its response and reset a
 * live ticket. Keying wide costs two integers per edited field.
 */
export function fieldWriteTarget(itemId: string, fieldKey: string): string {
	// The separator is written as the ESCAPE, never as the byte. A raw NUL in
	// the source makes the file binary to every text tool: `file` reports
	// "data", grep returns NOTHING AND EXIT 1 rather than "0 matches", and a
	// verification grep over it therefore reads as "absent" for every pattern.
	// This file shipped with the byte for one commit and silently answered "no"
	// to greps for symbols it contained.
	return `${itemId}\u0000${fieldKey}`;
}

/**
 * Re-derive a WHOLE-LIST write against a freshly-read row (BUG-3038).
 *
 * `submitOrderedOCC` re-sends its body after a conflict. That is right for a
 * scalar, whose gesture IS its value, and wrong for a list: the body is the
 * RESULT of a gesture applied to an older list, so re-sending it erases every
 * change another writer made in between. The field held [A,B,C], the user
 * removed A and [B,C] was sent, someone else added D, and the retry wrote [B,C]
 * back over [A,B,C,D].
 *
 * The gesture is recoverable from what the caller already has, so the write
 * contract does not need to change. `base` is the list on the row the write was
 * DISPATCHED against, captured at dispatch; `sent` is what the gesture produced
 * from it. Removed = base - sent, added = sent - base, and those are applied to
 * `fresh`:
 *
 *   - ORDER comes from `fresh`, so another writer's arrangement survives;
 *   - removed elements are dropped from it by identity;
 *   - added elements are appended in `sent`'s order, unless already present.
 *     An added element a third writer has since REMOVED is appended again: our
 *     gesture was to add it, and that is its honest outcome.
 *
 * The base is the ROW, not the list the editor showed, on purpose. While an
 * earlier write of this pane is in flight the editor holds that write's result
 * and derives the next gesture from it, so `sent` already carries both. The
 * row does not, so its delta names BOTH gestures, which is right when the
 * earlier one never commits (a superseded retry is abandoned). The editor's list
 * as base would name only the later gesture and bring the earlier removal back
 * (codex round 2 on BUG-3038 proposed it; the test beside this pins why not).
 *
 * There is no reorder gesture in the pane today (a list is only ever added to
 * or removed from). If one is added, a pure reorder has an empty delta and this
 * returns `fresh` unchanged, so it would need its own arm here.
 *
 * Elements are compared with `===`. A multi_relation list holds stored ids,
 * and the pane builds `sent` from those same ids plus a picked item's id, so
 * all three lists share one spelling. A missing or non-array value reads as
 * the empty list.
 */
export function rederiveListWrite(base: unknown, sent: unknown, fresh: unknown): unknown[] {
	const list = (v: unknown): unknown[] => (Array.isArray(v) ? v : []);
	const b = list(base);
	const s = list(sent);
	const removed = b.filter((x) => !s.includes(x));
	const added = s.filter((x) => !b.includes(x));
	const out = list(fresh).filter((x) => !removed.includes(x));
	for (const x of added) if (!out.includes(x)) out.push(x);
	return out;
}
