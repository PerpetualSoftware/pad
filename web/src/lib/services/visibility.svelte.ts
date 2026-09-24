/**
 * Tab visibility service — notifies subscribers when the browser tab
 * regains focus after being hidden. Used to refresh stale data when
 * SSE events may have been lost while the tab was in the background.
 */

type Callback = () => void;

const listeners = new Set<Callback>();
let initialized = false;
let lastResumeTime = 0;

const THROTTLE_MS = 2000;

function init() {
	if (initialized || typeof document === 'undefined') return;
	initialized = true;

	document.addEventListener('visibilitychange', () => {
		if (!document.hidden) {
			const now = Date.now();
			if (now - lastResumeTime < THROTTLE_MS) return;
			lastResumeTime = now;

			for (const cb of listeners) {
				try {
					cb();
				} catch {
					// Don't let one failing callback break others
				}
			}
		}
	});
}

function onTabResume(cb: Callback): () => void {
	listeners.add(cb);
	return () => {
		listeners.delete(cb);
	};
}

/**
 * Has this page been VISIBLE at least once? (BUG-3192 Unit B.)
 *
 * A tab restored by the browser, or opened with a middle-click, loads hidden,
 * and several of them load at once against one per-user request budget.
 * Surfaces gate reads nobody can see yet on this, so a hidden load spends the
 * budget on its essentials only and the rest follow when it is first shown.
 *
 * ONE-WAY: it flips false -> true on the first `visibilitychange` to visible
 * and never back, so anything mounted behind it is never torn down by a later
 * hide. Reactive (`$state`), so a template gate re-renders when it flips.
 * Armed at import rather than in `init()`, because a gated surface can mount
 * before anything calls `init()`.
 */
let seenVisible = $state(typeof document === 'undefined' || !document.hidden);

function armSeenVisible() {
	if (seenVisible || typeof document === 'undefined') return;
	const onChange = () => {
		if (document.hidden) return;
		seenVisible = true;
		document.removeEventListener('visibilitychange', onChange);
	};
	document.addEventListener('visibilitychange', onChange);
}
armSeenVisible();

/** Test seam: start over as a page loaded hidden (or visible). */
function resetSeenVisibleForTest(hidden: boolean) {
	seenVisible = !hidden;
	armSeenVisible();
}

export const visibility = {
	init,
	onTabResume,
	get seenVisible() {
		return seenVisible;
	},
	resetSeenVisibleForTest,
};
