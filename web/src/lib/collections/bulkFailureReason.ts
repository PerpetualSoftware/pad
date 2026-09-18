import type { BulkItemFailure } from '$lib/types';

/**
 * Builds the "why" half of a bulk-operation toast (BUG-3102).
 *
 * The collection page used to compute a failure COUNT and throw the reasons
 * away: `runBulkOn` read `res.updated` and derived `failed` by subtraction,
 * so a row that the server refused with a structured `code` + `details` —
 * `plan_limit_exceeded`, `open_children` — reached the user as the bare
 * "N failed". The user was told that something did not happen and never told
 * what would let them fix it.
 *
 * ## Why this reads `error` and not `details`
 *
 * The server already sends the finished sentence. `handlers_items_bulk.go`
 * builds a refused row as `message: planLimitMessage(&ple.Result)`, which is
 * byte-for-byte the string the single-restore 403 produces, and that string
 * lands in `failed[].error`. `$lib/api/client`'s own `planLimitMessage` follows
 * the same rule for the 403 path and says why: the server owns the wording so
 * there is ONE source of truth for it (TASK-788).
 *
 * Rebuilding the sentence here out of `details.limit` / `details.current` was
 * the original plan for this fix and is deliberately not what it does. It would
 * fork the wording into TypeScript, drift the next time the server's phrasing
 * moves, and it is already unable to produce the right answer: BUG-3103's
 * import refusal reads "This import would add N items, over the M-item limit…",
 * which is not derivable from limit/current without reimplementing the server's
 * branch on `requested`. One refusal would then read differently depending on
 * whether the user reached it through bulk or through a single restore.
 *
 * So: **`code` decides, `error` displays.** `details` is left for callers that
 * want to branch on the numbers — an upgrade prompt, say — not to build prose.
 *
 * ## Why `unexplained` is a separate input
 *
 * `runBulkOn` chunks its ids, and a chunk that THROWS breaks the loop: its
 * items never reach the server's per-row reporting, so they are failures with
 * no `failed[]` row at all. They must not borrow a reason that belongs to a
 * different row — a plan-limit sentence attached to items that were never
 * attempted would send the user to fix the wrong thing. They are counted and
 * described as not attempted.
 */
export interface BulkFailureSummary {
	/** Total rows that did not succeed, explained or not. */
	total: number;
	/**
	 * One clause per DISTINCT reason, in first-seen order, each already
	 * carrying its own count. Empty when nothing was explained.
	 */
	reasons: string[];
	/** Rows that failed with no row of their own (a thrown chunk). */
	unexplained: number;
}

/**
 * Groups failure rows by their reason so a mixed result reports each reason
 * once with a count, rather than once per row.
 *
 * Grouping is by the pair (`code`, `error`), not by `code` alone: two rows can
 * share `open_children` and name different blocking children, and collapsing
 * those would state one row's specifics over the other's. Rows with no `code`
 * still group by their message, which is what makes an ordinary "item not found
 * or not archived" batch read as one clause instead of five.
 *
 * @param failed rows the server reported, accumulated across every chunk
 * @param unexplained failures with no row (items in a chunk that threw)
 */
export function summarizeBulkFailures(
	failed: BulkItemFailure[],
	unexplained: number
): BulkFailureSummary {
	const order: string[] = [];
	const counts = new Map<string, { count: number; message: string }>();

	for (const row of failed) {
		const message = (row.error ?? '').trim();
		if (!message) continue; // nothing to say about this row
		// JSON.stringify rather than a delimiter: a separator character has to
		// be one that cannot occur in a code or a message, and the obvious
		// choice — a NUL — is a LITERAL NUL BYTE in the source once written,
		// which makes the file read as BINARY to grep (`-I`), so a later
		// search for a symbol in here silently returns nothing. A pair
		// encoded as JSON has no such character and cannot collide.
		const key = JSON.stringify([row.code ?? '', message]);
		const seen = counts.get(key);
		if (seen) {
			seen.count += 1;
		} else {
			counts.set(key, { count: 1, message });
			order.push(key);
		}
	}

	const reasons = order.map((key) => {
		const { count, message } = counts.get(key)!;
		return count > 1 ? `${count} × ${message}` : message;
	});

	return { total: failed.length + unexplained, reasons, unexplained };
}

/**
 * Renders the whole toast sentence for a bulk run.
 *
 * `verb` is the caller's past-tense word ("Restored", "Archived", "Moved"), so
 * this stays the one place the sentence is assembled for every bulk op rather
 * than only for restore.
 */
export function bulkToastMessage(
	verb: string,
	ok: number,
	summary: BulkFailureSummary,
	opts?: { synced?: boolean; notAttemptedReason?: string }
): string {
	const noun = (n: number) => `${n} item${n !== 1 ? 's' : ''}`;
	const tail = summary.reasons.length > 0 ? ` — ${summary.reasons.join('; ')}` : '';
	// Named separately from the per-row reasons so it can never read as one of
	// them. `notAttemptedReason` is the THROWN CHUNK'S OWN error, which is a
	// legitimate reason for exactly these rows — it is not another row's reason
	// being borrowed, which is the thing this separation exists to prevent. It
	// is carried because the code this replaces showed it (as the whole toast)
	// and dropping it would lose the only description of a transport failure.
	const why = opts?.notAttemptedReason?.trim();
	const notAttempted =
		summary.unexplained > 0
			? `${summary.reasons.length > 0 ? '; ' : ' — '}${noun(summary.unexplained)} not attempted${why ? `: ${why}` : ''}`
			: '';

	if (ok === 0) {
		// Nothing succeeded, so lead with the failure rather than "Restored 0".
		//
		// PASSIVE, because `verb` is PAST TENSE at every call site ("Restored",
		// "Archived", "Moved", "Moved back"). The code this replaces read
		// `Failed to ${verb.toLowerCase()} items`, which renders "Failed to
		// restored items" — a pre-existing grammar bug that only ever surfaced
		// in the all-failed branch nobody had seen. "N items could not be
		// restored" composes correctly with every verb in use, including the
		// two-word "Moved back".
		const head = `${noun(summary.total)} could not be ${verb.toLowerCase()}`;
		return `${head}${tail}${notAttempted}`;
	}
	if (summary.total > 0) {
		return `${verb} ${noun(ok)}, ${summary.total} failed${tail}${notAttempted}`;
	}
	return `${verb} ${noun(ok)}${opts?.synced === false ? ' (updating…)' : ''}`;
}
