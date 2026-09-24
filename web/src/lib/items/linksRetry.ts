/**
 * A scheduled retry for an item pane's relationship links (BUG-2992).
 *
 * WHY THIS EXISTS. A failed links refresh keeps the last-good rows on screen
 * (`refreshLinksPreservingOnFailure` in ItemDetail, BUG-2871), which is right,
 * and it is also quiet. Nothing else corrects them:
 *
 *  - A link added or removed elsewhere, without touching the item, emits no
 *    item event, so no later refresh is guaranteed.
 *  - Holding back the sync cursor cannot help either. `markSynced()` is one
 *    shared `lastSyncTime = Date.now()`, and the workspace layout advances it
 *    on its own after every clean reconcile, so a pane withholding its call
 *    still sees the cursor move.
 *
 * So the pane has to remember the failure itself, and this is that memory: one
 * pending target per pane, retried on a bounded backoff, and restarted by
 * `kick()` (the pane calls it on each sync result, i.e. on tab resume) once the
 * backoff is spent.
 *
 * The module only SCHEDULES. `attempt` (ItemDetail's `retryLinks`, which keeps
 * the commit where the identity-fence gate can see it) does the fetch, the
 * fences and the install, and answers one question: are this target's links
 * fresh now?
 * `true` when it installed them, or when the target no longer applies (the pane
 * switched items or was destroyed). `false` when the fetch failed OR a newer
 * write got there first. The newer write may not have been a successful fetch:
 * an SSE re-read that failed on the item GET bumps the same generation and
 * never reaches the links, so treating "superseded" as "fresh" would drop the
 * retry with nothing left to correct it.
 */
export interface LinksRetryTarget {
	itemId: string;
	ws: string;
}

/**
 * The backoff, in ms. Three tries over about 72 seconds, then `kick()` or a new
 * failure restarts it. It is not an indefinite poll, because a persistently
 * failing endpoint would otherwise be hit once a minute for as long as the
 * pane stays open.
 */
export const LINKS_RETRY_DELAYS_MS: readonly number[] = [2_000, 10_000, 60_000];

export interface LinksRetry {
	/** A links refresh for `target` failed; make sure a retry is scheduled. */
	failed(target: LinksRetryTarget): void;
	/** A links refresh for `itemId` succeeded; nothing is owed for it. */
	succeeded(itemId: string): void;
	/** Retry now, restarting a spent backoff. No-op when nothing is owed. */
	kick(): void;
	/** Drop everything (pane destroyed). */
	cancel(): void;
	/** Whether a retry is owed. */
	readonly pending: boolean;
}

export function createLinksRetry(
	attempt: (target: LinksRetryTarget) => Promise<boolean>,
	delays: readonly number[] = LINKS_RETRY_DELAYS_MS,
): LinksRetry {
	let target: LinksRetryTarget | null = null;
	let step = 0;
	let timer: ReturnType<typeof setTimeout> | undefined;
	let running = false;

	function clearTimer() {
		if (timer !== undefined) {
			clearTimeout(timer);
			timer = undefined;
		}
	}

	function arm() {
		clearTimer();
		if (!target || step >= delays.length) return;
		timer = setTimeout(() => void fire(), delays[step]);
		step++;
	}

	async function fire() {
		timer = undefined;
		const t = target;
		if (!t || running) return;
		running = true;
		let fresh = false;
		try {
			fresh = await attempt(t);
		} catch {
			fresh = false;
		} finally {
			running = false;
		}
		if (target !== t) {
			// Replaced while in flight: a new failure (keep going, for the NEW
			// target) or a success/cancel (target is null, nothing owed).
			if (target) arm();
			return;
		}
		if (fresh) {
			target = null;
			step = 0;
		} else {
			arm();
		}
	}

	return {
		failed(t) {
			const idle = timer === undefined && !running;
			const otherItem = !target || target.itemId !== t.itemId;
			// A different item, or a schedule that is spent or not started,
			// begins again from the first delay; a timer armed for the previous
			// item's backoff is replaced (`arm` clears it). A live schedule for
			// the same item keeps its place, so repeated failures back off
			// rather than reset. An attempt in flight re-arms when it returns.
			if (otherItem || idle) step = 0;
			target = t;
			if ((otherItem || idle) && !running) arm();
		},
		succeeded(itemId) {
			if (target?.itemId !== itemId) return;
			target = null;
			step = 0;
			clearTimer();
		},
		kick() {
			if (!target || running) return;
			clearTimer();
			step = 0;
			void fire();
		},
		cancel() {
			target = null;
			step = 0;
			clearTimer();
		},
		get pending() {
			return target !== null;
		},
	};
}

