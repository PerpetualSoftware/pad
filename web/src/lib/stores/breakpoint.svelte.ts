import { browser } from '$app/environment';

/**
 * The single mobile breakpoint for the whole app (px). Viewports whose width
 * is at or below this are "mobile".
 *
 * 768 was chosen as the canonical value (TASK-2028 / PLAN-1984) because it is
 * the width the app chrome already switches at — `uiStore.isMobile` drives the
 * Sidebar / TopBar / BottomNav / MobileContextBar at ≤768, and the dominant CSS
 * `@media (max-width: 768px)` queries key off the same width. The component-local
 * bottom-sheet swaps that previously used 639.98px were pure-JS gates (no CSS
 * media query at 639.98), so aligning them up to 768 makes JS `isMobile` agree
 * with the CSS layout switch without any CSS rewrite. Before this unification the
 * two breakpoints disagreed in the 640–768px band, producing e.g. a desktop
 * dropdown rendered inside otherwise-mobile chrome.
 */
export const MOBILE_BREAKPOINT = 768;

/** Canonical media-query string. Kept in lockstep with the CSS `@media (max-width: 768px)`. */
export const MOBILE_MEDIA_QUERY = `(max-width: ${MOBILE_BREAKPOINT}px)`;

// Single app-wide source of truth for the mobile viewport flag. Written only by
// the one matchMedia listener below; components READ it (directly or via
// `uiStore.isMobile`). Deliberately no `$effect` here — per CONVE-1688 the store
// must never read state it also writes, which would wedge the effect scheduler.
// SSR-safe: `false` on the server, resolved to the real value at module init on
// the client (the same browser-guarded pattern uiStore uses).
let mobile = $state(browser ? window.matchMedia(MOBILE_MEDIA_QUERY).matches : false);

if (browser) {
	// One listener for the entire app instead of one per component. `change`
	// fires only when the viewport crosses the breakpoint, not on every resize.
	window.matchMedia(MOBILE_MEDIA_QUERY).addEventListener('change', (e) => {
		mobile = e.matches;
	});
}

/**
 * Reactive mobile-viewport flag. Read it in components (`viewport.isMobile`) or
 * templates and it tracks the viewport automatically. `false` during SSR.
 */
export const viewport = {
	get isMobile() {
		return mobile;
	}
};
