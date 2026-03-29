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

export const visibility = { init, onTabResume };
