import { browser } from '$app/environment';
import { viewport, MOBILE_MEDIA_QUERY } from '$lib/stores/breakpoint.svelte';

let sidebarOpen = $state(browser ? !viewport.isMobile : true);
let topbarOpen = $state(browser ? localStorage.getItem('pad-topbar') !== 'closed' : true);
let searchOpen = $state(false);
let isTouch = $state(browser ? 'ontouchstart' in window : false);
// True while the on-screen keyboard is up. Detected from the geometry of the
// keyboard itself — visualViewport.height shrinking below the tallest height
// we've seen (the keyboard-closed baseline). This works on iOS Safari AND
// Android Chrome; a naive `innerHeight - visualViewport.height` does NOT,
// because those browsers shrink window.innerHeight in lockstep with the visual
// viewport, leaving the delta ~0. Consumers (e.g. BottomNav) hide fixed bottom
// chrome so it doesn't sit stranded above the keyboard. PLAN-1694.
let keyboardVisible = $state(false);
let detailPanelOpen = $state(browser ? localStorage.getItem('pad-detail-panel') !== 'closed' && !viewport.isMobile : false);
let createWorkspaceOpen = $state(false);
let quickAddRequested = $state(false);
let quickAddTargetSlug = $state<string | null>(null);
let collectionSearchHandler = $state<(() => void) | null>(null);
// Slug of a workspace whose Connect modal should auto-open as soon as the
// user lands on its workspace page. Set by CreateWorkspaceModal (via
// +layout.svelte's onWorkspaceCreated wire-up) right before `goto`; consumed
// by the workspace +page.svelte when its slug matches. Mirrors the
// request/clear pattern used by quickAdd above. PLAN-1519 / TASK-1526.
let connectAfterNavigateSlug = $state<string | null>(null);

if (browser) {
	// `isMobile` itself is owned by the shared breakpoint store (one app-wide
	// listener). Here we only run the layout side effects that must fire when
	// the viewport crosses the mobile breakpoint: collapse the sidebar/detail
	// panel entering mobile, restore the sidebar leaving it. `change` fires only
	// on a crossing, so no manual before/after comparison is needed.
	//
	// `detailPanelOpen` is the pre-PLAN-2105 legacy panel boolean — it is
	// mutated here but no component reads it to render anything anymore, so
	// this force-close is inert. The collection page's split-pane detail view
	// (PLAN-2105 / TASK-2121) is deliberately NOT wired to it: pane state is
	// URL-derived (`?item=`), and on mobile the pane simply restyles to a
	// full-screen overlay via CSS (see the `@media (max-width: 768px)` block
	// in the collection +page.svelte). Routing pane visibility through this
	// boolean would silently drop `?item=` on every mobile-entry crossing —
	// the exact bug TASK-2121 must avoid. If a future consumer starts reading
	// `detailPanelOpen`, keep this handler's reach limited to that legacy
	// panel and off the URL-derived pane.
	window.matchMedia(MOBILE_MEDIA_QUERY).addEventListener('change', (e) => {
		if (e.matches) {
			sidebarOpen = false;
			detailPanelOpen = false;
		} else {
			sidebarOpen = true;
		}
	});

	// Track on-screen keyboard visibility from the visual viewport. Only touch
	// devices raise a soft keyboard, so gate on isTouch — a narrow desktop
	// window never has one and must not hide the nav.
	if (isTouch && window.visualViewport) {
		const vv = window.visualViewport;
		// The tallest viewport height we've seen == keyboard closed. It grows as
		// the URL bar collapses on scroll and resets on rotation, so the keyboard
		// shrink is always measured against the true full-height reference.
		let baseline = vv.height;
		const KEYBOARD_MIN_PX = 150; // smaller shrinks are browser chrome, not a keyboard
		const measure = () => {
			if (vv.height > baseline) baseline = vv.height;
			keyboardVisible = baseline - vv.height > KEYBOARD_MIN_PX;
		};
		vv.addEventListener('resize', measure);
		vv.addEventListener('scroll', measure);
		// Re-capture the baseline after an orientation change so a shorter
		// landscape viewport isn't mistaken for an open keyboard.
		window.addEventListener('orientationchange', () => {
			baseline = 0;
			setTimeout(measure, 300);
		});
	}
}

export const uiStore = {
	get sidebarOpen() { return sidebarOpen; },
	get topbarOpen() { return topbarOpen; },
	get searchOpen() { return searchOpen; },
	get isMobile() { return viewport.isMobile; },
	get isTouch() { return isTouch; },
	get keyboardVisible() { return keyboardVisible; },
	get detailPanelOpen() { return detailPanelOpen; },
	get createWorkspaceOpen() { return createWorkspaceOpen; },

	toggleSidebar() { sidebarOpen = !sidebarOpen; },
	openSidebar() { sidebarOpen = true; },
	closeSidebar() { sidebarOpen = false; },

	toggleTopbar() {
		topbarOpen = !topbarOpen;
		if (browser) localStorage.setItem('pad-topbar', topbarOpen ? 'open' : 'closed');
	},
	openTopbar() {
		topbarOpen = true;
		if (browser) localStorage.setItem('pad-topbar', 'open');
	},
	closeTopbar() {
		topbarOpen = false;
		if (browser) localStorage.setItem('pad-topbar', 'closed');
	},
	openSearch() { searchOpen = true; },
	closeSearch() { searchOpen = false; },
	toggleSearch() { searchOpen = !searchOpen; },

	toggleDetailPanel() {
		detailPanelOpen = !detailPanelOpen;
		if (browser) localStorage.setItem('pad-detail-panel', detailPanelOpen ? 'open' : 'closed');
	},
	openDetailPanel() {
		detailPanelOpen = true;
		if (browser) localStorage.setItem('pad-detail-panel', 'open');
	},
	closeDetailPanel() {
		detailPanelOpen = false;
		if (browser) localStorage.setItem('pad-detail-panel', 'closed');
	},

	openCreateWorkspace() { createWorkspaceOpen = true; },
	closeCreateWorkspace() { createWorkspaceOpen = false; },

	// Quick-add item trigger — sidebar watches this
	get quickAddRequested() { return quickAddRequested; },
	get quickAddTargetSlug() { return quickAddTargetSlug; },
	requestQuickAdd(collectionSlug?: string) { quickAddTargetSlug = collectionSlug ?? null; quickAddRequested = true; },
	clearQuickAddRequest() { quickAddRequested = false; quickAddTargetSlug = null; },

	// Connect-modal auto-open signal (PLAN-1519 / TASK-1526). Fired by
	// CreateWorkspaceModal after a successful create/import, consumed by
	// the workspace +page.svelte when the user lands on the matching
	// workspace. Always read via `consumeConnectAfterNavigate()` so the
	// signal is single-shot — re-reading the value would leave the request
	// stuck and reopen the modal on every reactive re-run.
	get connectAfterNavigateSlug() { return connectAfterNavigateSlug; },
	requestConnectAfterNavigate(slug: string) { connectAfterNavigateSlug = slug; },
	consumeConnectAfterNavigate(): string | null {
		const slug = connectAfterNavigateSlug;
		connectAfterNavigateSlug = null;
		return slug;
	},

	// Collection search (Cmd+F) registry — pages that want to handle Cmd+F
	// register a handler on mount and unregister on destroy. The layout's
	// global keydown handler only calls preventDefault when a handler is
	// registered, so on pages without one (e.g. item view) Cmd+F falls
	// through to the browser's native find. (BUG-986)
	get hasCollectionSearchHandler() { return collectionSearchHandler !== null; },
	triggerCollectionSearch() { collectionSearchHandler?.(); },
	registerCollectionSearch(handler: () => void) { collectionSearchHandler = handler; },
	unregisterCollectionSearch() { collectionSearchHandler = null; },

	/** Close sidebar on mobile after navigation */
	onNavigate() {
		if (viewport.isMobile) sidebarOpen = false;
	},
};
