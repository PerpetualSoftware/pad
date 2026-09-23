/**
 * A rate-capped, trailing refresh trigger for views that re-read a feed's head
 * on a NOISY event (BUG-3160). `item_updated` fires on every item PATCH,
 * including the collaborative editor's ~5s content flush, so a view that
 * re-read on each one reintroduced the jitter and rate-limit errors that got
 * the event excluded in the first place. This coalesces a burst into at most
 * one call per `intervalMs`, always followed by one call AFTER the last event
 * (the trailing edge), so the final state of a burst is always read.
 *
 * - A lone event after quiet waits `minDelayMs` (to coalesce a replay burst),
 *   then runs.
 * - Within `intervalMs` of the last run, the next run waits out the rest of the
 *   interval. At most one run is ever pending.
 * - While `isHidden()` is true nothing runs; the trigger is remembered, and
 *   `onVisible()` runs one catch-up refresh only if an event arrived while
 *   hidden (lead ruling on BUG-3160).
 * - `dispose()` clears the timer; nothing runs after it.
 */
export interface ThrottledRefresh {
	trigger(): void;
	onVisible(): void;
	dispose(): void;
}

export interface ThrottledRefreshOptions {
	intervalMs: number;
	minDelayMs?: number;
	isHidden?: () => boolean;
	now?: () => number;
}

export function createThrottledRefresh(run: () => void, opts: ThrottledRefreshOptions): ThrottledRefresh {
	const { intervalMs, minDelayMs = 500 } = opts;
	const isHidden = opts.isHidden ?? (() => typeof document !== 'undefined' && document.visibilityState === 'hidden');
	const now = opts.now ?? (() => Date.now());
	let timer: ReturnType<typeof setTimeout> | undefined;
	let lastRun = -Infinity;
	let missedWhileHidden = false;
	let disposed = false;

	function fire() {
		timer = undefined;
		if (disposed) return;
		if (isHidden()) {
			missedWhileHidden = true;
			return;
		}
		lastRun = now();
		run();
	}

	function trigger() {
		if (disposed) return;
		// No hidden check here: `fire` decides, at the moment a run would happen,
		// and records the miss. A second check here was unobservable (a mutant
		// removing it survived every test), so it is not kept.
		if (timer !== undefined) return; // one pending run covers this event too
		const delay = Math.max(minDelayMs, lastRun + intervalMs - now());
		timer = setTimeout(fire, delay);
	}

	function onVisible() {
		if (disposed || !missedWhileHidden) return;
		missedWhileHidden = false;
		trigger();
	}

	function dispose() {
		disposed = true;
		clearTimeout(timer);
		timer = undefined;
	}

	return { trigger, onVisible, dispose };
}
